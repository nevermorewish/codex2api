package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// 首字超时的请求体体积分档。档位是全局固定的，不再是一个"档位值"，
// 而是一行的键：每个模型各有一行，逐档覆盖全局档位表（见 FirstTokenTimeoutSettings）。
type FirstTokenSizeBracket string

const (
	FirstTokenSizeUnder50KB  FirstTokenSizeBracket = "under_50kb"
	FirstTokenSizeUnder100KB FirstTokenSizeBracket = "under_100kb"
	FirstTokenSizeUnder200KB FirstTokenSizeBracket = "under_200kb"
	FirstTokenSizeUnder500KB FirstTokenSizeBracket = "under_500kb"
	FirstTokenSizeOver500KB  FirstTokenSizeBracket = "over_500kb"
)

// FirstTokenSizeBrackets 是档位的展示/校验顺序，也是 Normalize 时补齐默认值的顺序。
var FirstTokenSizeBrackets = []FirstTokenSizeBracket{
	FirstTokenSizeUnder50KB,
	FirstTokenSizeUnder100KB,
	FirstTokenSizeUnder200KB,
	FirstTokenSizeUnder500KB,
	FirstTokenSizeOver500KB,
}

// FirstTokenSizeBracketLimitBytes 返回该档位的上界（字节），最后一档无上界返回 0。
// proxy 侧按同一张表判定请求体落在哪一档，避免阈值在两处各写一份。
func FirstTokenSizeBracketLimitBytes(bracket FirstTokenSizeBracket) int {
	switch bracket {
	case FirstTokenSizeUnder50KB:
		return 50 * 1024
	case FirstTokenSizeUnder100KB:
		return 100 * 1024
	case FirstTokenSizeUnder200KB:
		return 200 * 1024
	case FirstTokenSizeUnder500KB:
		return 500 * 1024
	}
	return 0
}

var firstTokenSizeBracketSet = map[FirstTokenSizeBracket]struct{}{
	FirstTokenSizeUnder50KB:  {},
	FirstTokenSizeUnder100KB: {},
	FirstTokenSizeUnder200KB: {},
	FirstTokenSizeUnder500KB: {},
	FirstTokenSizeOver500KB:  {},
}

// FirstTokenSizeBracketAt 返回请求体积落在哪一档。判定与档位表同源，
// 边界取"小于上界"，与前端表格标签（< 50 KB / 50 ≤ KB < 100）一致。
func FirstTokenSizeBracketAt(bodySize int) FirstTokenSizeBracket {
	for _, bracket := range FirstTokenSizeBrackets {
		limit := FirstTokenSizeBracketLimitBytes(bracket)
		if limit == 0 || bodySize < limit {
			return bracket
		}
	}
	return FirstTokenSizeOver500KB
}

const (
	minFirstTokenTimeoutSeconds = 1
	maxFirstTokenTimeoutSeconds = 600
)

// FirstTokenModelTimeouts 是一个模型的五档超时（秒）。缺档的模型在归一化时
// 用全局档位表补齐，因此运行时拿到的映射永远是完整五行。
type FirstTokenModelTimeouts map[FirstTokenSizeBracket]int

// FirstTokenTimeoutSettings 是全局档位表（未单独配置的模型都用它）加上
// 按模型逐档覆盖的表。
type FirstTokenTimeoutSettings struct {
	Under50KB     int                                `json:"under_50kb"`
	Under100KB    int                                `json:"under_100kb"`
	Under200KB    int                                `json:"under_200kb"`
	Under500KB    int                                `json:"under_500kb"`
	Over500KB     int                                `json:"over_500kb"`
	ModelTimeouts map[string]FirstTokenModelTimeouts `json:"model_timeouts"`
}

type FirstTokenTimeoutSettingsInput struct {
	Under50KB     int                                `json:"under_50kb"`
	Under100KB    int                                `json:"under_100kb"`
	Under200KB    int                                `json:"under_200kb"`
	Under500KB    int                                `json:"under_500kb"`
	Over500KB     int                                `json:"over_500kb"`
	ModelTimeouts map[string]FirstTokenModelTimeouts `json:"model_timeouts"`
}

func DefaultFirstTokenTimeoutSettings() FirstTokenTimeoutSettings {
	return FirstTokenTimeoutSettings{
		Under50KB:     10,
		Under100KB:    20,
		Under200KB:    30,
		Under500KB:    50,
		Over500KB:     90,
		ModelTimeouts: map[string]FirstTokenModelTimeouts{},
	}
}

// Validate 要求全局五档完整且落在 1..600。模型表允许缺档（缺档在 Normalize 时
// 继承全局值），但已填的值同样必须合法——这里显式校验，避免只在落库时被静默丢弃。
func (d FirstTokenTimeoutSettings) Validate() error {
	for _, value := range []int{d.Under50KB, d.Under100KB, d.Under200KB, d.Under500KB, d.Over500KB} {
		if value < minFirstTokenTimeoutSeconds || value > maxFirstTokenTimeoutSeconds {
			return fmt.Errorf("first token timeout values must be 1..600 seconds")
		}
	}
	for model, timeouts := range d.ModelTimeouts {
		if strings.TrimSpace(model) == "" {
			return fmt.Errorf("first token timeout model name must not be blank")
		}
		for bracket, value := range timeouts {
			if _, ok := firstTokenSizeBracketSet[bracket]; !ok {
				return fmt.Errorf("unknown first token size bracket %q for model %s", bracket, model)
			}
			if value < minFirstTokenTimeoutSeconds || value > maxFirstTokenTimeoutSeconds {
				return fmt.Errorf("first token timeout for model %s must be 1..600 seconds", model)
			}
		}
	}
	return nil
}

// NormalizeFirstTokenTimeoutSettings 把全局档位补到默认值、清理模型表：
// 模型名去空白转小写（与 proxy 侧查找键一致）、未知档位与越界值丢弃（丢弃后
// 该档回落到全局值，不会让模型整行失效），最后把每个模型的缺档用全局值补齐。
func NormalizeFirstTokenTimeoutSettings(d FirstTokenTimeoutSettings) FirstTokenTimeoutSettings {
	defaults := DefaultFirstTokenTimeoutSettings()
	values := []*int{&d.Under50KB, &d.Under100KB, &d.Under200KB, &d.Under500KB, &d.Over500KB}
	fallbacks := []int{defaults.Under50KB, defaults.Under100KB, defaults.Under200KB, defaults.Under500KB, defaults.Over500KB}
	for i, value := range values {
		if *value < minFirstTokenTimeoutSeconds || *value > maxFirstTokenTimeoutSeconds {
			*value = fallbacks[i]
		}
	}
	global := map[FirstTokenSizeBracket]int{
		FirstTokenSizeUnder50KB:  d.Under50KB,
		FirstTokenSizeUnder100KB: d.Under100KB,
		FirstTokenSizeUnder200KB: d.Under200KB,
		FirstTokenSizeUnder500KB: d.Under500KB,
		FirstTokenSizeOver500KB:  d.Over500KB,
	}
	normalized := make(map[string]FirstTokenModelTimeouts, len(d.ModelTimeouts))
	for model, timeouts := range d.ModelTimeouts {
		key := NormalizeFirstTokenModelKey(model)
		if key == "" {
			continue
		}
		row := make(FirstTokenModelTimeouts, len(global))
		for _, bracket := range FirstTokenSizeBrackets {
			if value, ok := timeouts[bracket]; ok && value >= minFirstTokenTimeoutSeconds && value <= maxFirstTokenTimeoutSeconds {
				row[bracket] = value
				continue
			}
			row[bracket] = global[bracket]
		}
		normalized[key] = row
	}
	d.ModelTimeouts = normalized
	return d
}

// NormalizeFirstTokenModelKey 是模型查找键的唯一来源：去空白、转小写。
// 上游模型名大小写不稳定，配置键与查找键必须用同一套规则，否则配置静默失效。
func NormalizeFirstTokenModelKey(model string) string {
	return strings.ToLower(strings.TrimSpace(model))
}

func (d FirstTokenTimeoutSettings) toInput() FirstTokenTimeoutSettingsInput {
	return FirstTokenTimeoutSettingsInput{
		Under50KB:     d.Under50KB,
		Under100KB:    d.Under100KB,
		Under200KB:    d.Under200KB,
		Under500KB:    d.Under500KB,
		Over500KB:     d.Over500KB,
		ModelTimeouts: d.ModelTimeouts,
	}
}

// modelTimeoutsFromRaw 解析 first_token_timeout_models 列。历史格式是
// {"model": 秒数} 的单值表（该值被当时的 proxy 逻辑当成整个模型的首字预算）。
// 单值表在逐档覆盖下没有对应位置，直接丢弃并让模型回落到全局档位表——
// 保留一处空值比把一个语义不明的秒数摊到五档里更安全。
func modelTimeoutsFromRaw(raw string) (map[string]FirstTokenModelTimeouts, bool) {
	if strings.TrimSpace(raw) == "" {
		return map[string]FirstTokenModelTimeouts{}, false
	}
	var generic map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &generic); err != nil {
		return map[string]FirstTokenModelTimeouts{}, false
	}
	timeouts := make(map[string]FirstTokenModelTimeouts, len(generic))
	legacy := false
	for model, payload := range generic {
		var row FirstTokenModelTimeouts
		if err := json.Unmarshal(payload, &row); err == nil && len(row) > 0 {
			timeouts[model] = row
			continue
		}
		// 单值格式（含 0、null 等无法解析成对象的值）按历史数据处理。
		legacy = true
	}
	return timeouts, legacy
}

func (db *DB) GetFirstTokenTimeoutSettings(ctx context.Context) (FirstTokenTimeoutSettings, error) {
	d := DefaultFirstTokenTimeoutSettings()
	var raw sql.NullString
	err := db.conn.QueryRowContext(ctx, `SELECT COALESCE(first_token_timeout_under_50kb,10),COALESCE(first_token_timeout_under_100kb,20),COALESCE(first_token_timeout_under_200kb,30),COALESCE(first_token_timeout_under_500kb,50),COALESCE(first_token_timeout_over_500kb,90),COALESCE(first_token_timeout_models,'{}') FROM system_settings WHERE id=1`).Scan(&d.Under50KB, &d.Under100KB, &d.Under200KB, &d.Under500KB, &d.Over500KB, &raw)
	if err == sql.ErrNoRows {
		return NormalizeFirstTokenTimeoutSettings(d), nil
	}
	if err != nil {
		return d, err
	}
	timeouts, legacy := modelTimeoutsFromRaw(raw.String)
	d.ModelTimeouts = timeouts
	if legacy {
		// 单值表已无处安放，读时顺手清掉，避免它在库里长期以"看起来还有配置"的样子存在。
		if persistErr := db.UpdateFirstTokenTimeoutSettings(ctx, NormalizeFirstTokenTimeoutSettings(d)); persistErr != nil {
			return NormalizeFirstTokenTimeoutSettings(d), nil
		}
	}
	return NormalizeFirstTokenTimeoutSettings(d), nil
}

func (db *DB) UpdateFirstTokenTimeoutSettings(ctx context.Context, d FirstTokenTimeoutSettings) error {
	if err := d.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(d.ModelTimeouts)
	if err != nil {
		return err
	}
	_, err = db.conn.ExecContext(ctx, `INSERT INTO system_settings(id,first_token_timeout_under_50kb,first_token_timeout_under_100kb,first_token_timeout_under_200kb,first_token_timeout_under_500kb,first_token_timeout_over_500kb,first_token_timeout_models) VALUES(1,$1,$2,$3,$4,$5,$6) ON CONFLICT(id) DO UPDATE SET first_token_timeout_under_50kb=EXCLUDED.first_token_timeout_under_50kb,first_token_timeout_under_100kb=EXCLUDED.first_token_timeout_under_100kb,first_token_timeout_under_200kb=EXCLUDED.first_token_timeout_under_200kb,first_token_timeout_under_500kb=EXCLUDED.first_token_timeout_under_500kb,first_token_timeout_over_500kb=EXCLUDED.first_token_timeout_over_500kb,first_token_timeout_models=EXCLUDED.first_token_timeout_models`, d.Under50KB, d.Under100KB, d.Under200KB, d.Under500KB, d.Over500KB, string(raw))
	return err
}

func (db *DB) UpdateFirstTokenTimeoutMode(ctx context.Context, mode string) error {
	if mode != "request_size" && mode != "first_token" && mode != "disabled" {
		return fmt.Errorf("invalid first token timeout mode")
	}
	_, err := db.conn.ExecContext(ctx, `INSERT INTO system_settings(id,first_token_timeout_mode) VALUES(1,$1) ON CONFLICT(id) DO UPDATE SET first_token_timeout_mode=EXCLUDED.first_token_timeout_mode`, mode)
	return err
}
