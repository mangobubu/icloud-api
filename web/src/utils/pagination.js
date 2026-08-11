export const DEFAULT_PAGE_SIZE = 20;
export const MAX_PAGE_SIZE = 1000;
export const ALL_PAGE_SIZE = 0;

export const PAGE_SIZE_OPTIONS = Object.freeze([
  { value: 20, label: "20 条/页" },
  { value: 50, label: "50 条/页" },
  { value: 100, label: "100 条/页" },
  { value: 500, label: "500 条/页" },
  { value: 1000, label: "1000 条/页" },
  { value: ALL_PAGE_SIZE, label: "全部显示" },
]);

const PAGE_SIZE_VALUES = new Set(
  PAGE_SIZE_OPTIONS.map((option) => option.value),
);

export function normalizePageSize(value, fallback = DEFAULT_PAGE_SIZE) {
  const number = Number(value);
  if (Number.isFinite(number)) {
    const normalized = Math.trunc(number);
    if (PAGE_SIZE_VALUES.has(normalized)) return normalized;
  }
  return fallback;
}

export function isAllPageSize(value) {
  return normalizePageSize(value) === ALL_PAGE_SIZE;
}
