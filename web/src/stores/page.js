import { reactive } from "vue";

const page = reactive({ title: "", subtitle: "" });

export function setPageHeader(title, subtitle = "") {
  page.title = title || "";
  page.subtitle = subtitle || "";
  document.title = `${page.title ? `${page.title} - ` : ""}iCloud 隐私邮箱`;
}

export function usePageHeader() {
  return page;
}
