export async function writeClipboardText(text: string): Promise<void> {
  if (typeof navigator !== "undefined" && navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(text)
      return
    } catch {
      // Fall through to the legacy method when permissions or context block the API.
    }
  }

  if (typeof document === "undefined") throw new Error("Clipboard unavailable")
  const textarea = document.createElement("textarea")
  textarea.value = text
  textarea.setAttribute("readonly", "")
  textarea.style.position = "fixed"
  textarea.style.opacity = "0"
  textarea.style.pointerEvents = "none"
  document.body.appendChild(textarea)
  textarea.focus()
  textarea.select()
  textarea.setSelectionRange(0, textarea.value.length)
  let copied = false
  try {
    copied = document.execCommand("copy")
  } finally {
    document.body.removeChild(textarea)
  }
  if (!copied) throw new Error("Clipboard unavailable")
}




