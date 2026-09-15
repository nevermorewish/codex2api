package database

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
)

func TestFirstTokenSizeBracketAt(t *testing.T) {
	cases := []struct {
		size int
		want FirstTokenSizeBracket
	}{
		{0, FirstTokenSizeUnder50KB},
		{50*1024 - 1, FirstTokenSizeUnder50KB},
		{50 * 1024, FirstTokenSizeUnder100KB},
		{100*1024 - 1, FirstTokenSizeUnder100KB},
		{100 * 1024, FirstTokenSizeUnder200KB},
		{200*1024 - 1, FirstTokenSizeUnder200KB},
		{200 * 1024, FirstTokenSizeUnder500KB},
		{500*1024 - 1, FirstTokenSizeUnder500KB},
		{500 * 1024, FirstTokenSizeOver500KB},
		{8 << 20, FirstTokenSizeOver500KB},
	}
	for _, tc := range cases {
		if got := FirstTokenSizeBracketAt(tc.size); got != tc.want {
			t.Errorf("size %d: bracket = %q, want %q", tc.size, got, tc.want)
		}
	}
}

// 每个模型一行五档：缺档用全局档位补齐，越界值丢弃后同样回落到全局，
// 未知档位键与空白模型名直接不进入结果。
func TestNormalizeFirstTokenTimeoutSettingsFillsModelRows(t *testing.T) {
	in := FirstTokenTimeoutSettings{
		Under50KB: 11, Under100KB: 22, Under200KB: 33, Under500KB: 44, Over500KB: 55,
		ModelTimeouts: map[string]FirstTokenModelTimeouts{
			"  GPT-6-Astra  ": {
				FirstTokenSizeOver500KB:  300,
				FirstTokenSizeUnder200KB: 0, // 越界 → 回落全局
				"bogus_bracket":          99,
			},
			"   ": {FirstTokenSizeUnder50KB: 5}, // 空白模型名 → 整个丢弃
		},
	}
	got := NormalizeFirstTokenTimeoutSettings(in)

	want := FirstTokenModelTimeouts{
		FirstTokenSizeUnder50KB:  11,
		FirstTokenSizeUnder100KB: 22,
		FirstTokenSizeUnder200KB: 33,
		FirstTokenSizeUnder500KB: 44,
		FirstTokenSizeOver500KB:  300,
	}
	if len(got.ModelTimeouts) != 1 {
		t.Fatalf("model rows = %d, want 1: %#v", len(got.ModelTimeouts), got.ModelTimeouts)
	}
	if row := got.ModelTimeouts["gpt-6-astra"]; !reflect.DeepEqual(row, want) {
		t.Fatalf("gpt-6-astra row = %#v, want %#v", row, want)
	}
}

func TestFirstTokenTimeoutSettingsValidate(t *testing.T) {
	valid := DefaultFirstTokenTimeoutSettings()
	if err := valid.Validate(); err != nil {
		t.Fatalf("default settings must validate: %v", err)
	}

	outOfRange := DefaultFirstTokenTimeoutSettings()
	outOfRange.Under50KB = 0
	if err := outOfRange.Validate(); err == nil {
		t.Fatal("global bracket 0 must not validate")
	}

	// 模型表的值以前完全不校验，越界只会在落库时被静默丢弃。
	modelOutOfRange := DefaultFirstTokenTimeoutSettings()
	modelOutOfRange.ModelTimeouts = map[string]FirstTokenModelTimeouts{
		"gpt-6-astra": {FirstTokenSizeUnder50KB: 601},
	}
	if err := modelOutOfRange.Validate(); err == nil {
		t.Fatal("model bracket 601 must not validate")
	}

	unknownBracket := DefaultFirstTokenTimeoutSettings()
	unknownBracket.ModelTimeouts = map[string]FirstTokenModelTimeouts{
		"gpt-6-astra": {"nope": 30},
	}
	if err := unknownBracket.Validate(); err == nil {
		t.Fatal("unknown bracket must not validate")
	}

	blankModel := DefaultFirstTokenTimeoutSettings()
	blankModel.ModelTimeouts = map[string]FirstTokenModelTimeouts{"  ": {FirstTokenSizeUnder50KB: 30}}
	if err := blankModel.Validate(); err == nil {
		t.Fatal("blank model name must not validate")
	}
}

// 历史格式 {"model": 秒数} 无处安放（逐档覆盖下没有"整行一个值"的位置），
// 读时必须丢弃并让模型回落全局档位，而不是把一个语义不明的秒数摊到五档里。
func TestModelTimeoutsFromRawDropsLegacyScalars(t *testing.T) {
	timeouts, legacy := modelTimeoutsFromRaw(`{"gpt-6-astra":300}`)
	if !legacy {
		t.Fatal("legacy single-value map must be reported as legacy")
	}
	if len(timeouts) != 0 {
		t.Fatalf("legacy rows must be dropped, got %#v", timeouts)
	}

	timeouts, legacy = modelTimeoutsFromRaw(`{"gpt-6-astra":{"under_50kb":90,"over_500kb":300},"gpt-5.5":120}`)
	if !legacy {
		t.Fatal("mixed map must still report the legacy row")
	}
	if len(timeouts) != 1 {
		t.Fatalf("per-bracket rows = %d, want 1: %#v", len(timeouts), timeouts)
	}
	if got := timeouts["gpt-6-astra"][FirstTokenSizeOver500KB]; got != 300 {
		t.Fatalf("gpt-6-astra over_500kb = %d, want 300", got)
	}

	if timeouts, legacy = modelTimeoutsFromRaw(`{}`); legacy || len(timeouts) != 0 {
		t.Fatalf("empty map: timeouts=%#v legacy=%v", timeouts, legacy)
	}
	if timeouts, legacy = modelTimeoutsFromRaw(`not json`); legacy || len(timeouts) != 0 {
		t.Fatalf("invalid json: timeouts=%#v legacy=%v", timeouts, legacy)
	}
}

func TestSQLiteFirstTokenTimeoutSettingsRoundTrip(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "first-token-timeout.sqlite"))
	if err != nil {
		t.Fatalf("New sqlite: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	want := FirstTokenTimeoutSettings{
		Under50KB: 11, Under100KB: 22, Under200KB: 33, Under500KB: 44, Over500KB: 55,
		ModelTimeouts: map[string]FirstTokenModelTimeouts{
			"gpt-6-astra": {FirstTokenSizeOver500KB: 300},
		},
	}
	if err := db.UpdateFirstTokenTimeoutSettings(ctx, want); err != nil {
		t.Fatalf("UpdateFirstTokenTimeoutSettings: %v", err)
	}
	got, err := db.GetFirstTokenTimeoutSettings(ctx)
	if err != nil {
		t.Fatalf("GetFirstTokenTimeoutSettings: %v", err)
	}
	if got.Under50KB != 11 || got.Over500KB != 55 {
		t.Fatalf("global brackets = %#v", got)
	}
	row := got.ModelTimeouts["gpt-6-astra"]
	if row[FirstTokenSizeOver500KB] != 300 {
		t.Fatalf("gpt-6-astra over_500kb = %d, want 300", row[FirstTokenSizeOver500KB])
	}
	// 未覆盖的档位在归一化时已补成全局值。
	if row[FirstTokenSizeUnder50KB] != 11 {
		t.Fatalf("gpt-6-astra under_50kb = %d, want 11 (global)", row[FirstTokenSizeUnder50KB])
	}
}

// 库里存着历史单值表时，读到的是回落到全局档位的模型表，而不是一个会说话的秒数。
func TestSQLiteFirstTokenTimeoutLegacyRowIsDroppedOnRead(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "first-token-legacy.sqlite"))
	if err != nil {
		t.Fatalf("New sqlite: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	if _, err := db.conn.ExecContext(ctx, `UPDATE system_settings SET first_token_timeout_models = '{"gpt-6-astra":300}' WHERE id = 1`); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}
	got, err := db.GetFirstTokenTimeoutSettings(ctx)
	if err != nil {
		t.Fatalf("GetFirstTokenTimeoutSettings: %v", err)
	}
	if len(got.ModelTimeouts) != 0 {
		t.Fatalf("legacy rows must be dropped, got %#v", got.ModelTimeouts)
	}
	// 读路径顺手清库：不留下"看起来还有配置"的残留。
	savings, err := db.GetFirstTokenTimeoutSettings(ctx)
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	if len(savings.ModelTimeouts) != 0 {
		t.Fatalf("legacy row reappeared: %#v", savings.ModelTimeouts)
	}
}
