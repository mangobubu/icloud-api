function validDate(value) {
  if (!value) {
    return null;
  }
  const date = value instanceof Date ? value : new Date(value);
  return Number.isNaN(date.getTime()) ? null : date;
}

function pad(value) {
  return String(value).padStart(2, "0");
}

export function formatTime(value, { seconds = false } = {}) {
  const date = validDate(value);
  if (!date) {
    return "-";
  }
  const base = `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(
    date.getDate(),
  )} ${pad(date.getHours())}:${pad(date.getMinutes())}`;
  return seconds ? `${base}:${pad(date.getSeconds())}` : base;
}

export function compactRunes(value, limit = 80) {
  const normalized = String(value || "")
    .trim()
    .replace(/\s+/g, " ");
  const runes = Array.from(normalized);
  return runes.length <= limit
    ? normalized
    : `${runes.slice(0, limit - 1).join("")}…`;
}

export function utf8Length(value) {
  return new TextEncoder().encode(String(value || "")).length;
}
