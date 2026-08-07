const sectionBasePaths = [
  ["aliases", "/admin/aliases"],
  ["audit", "/admin/audit"],
  ["security", "/admin/security"],
];

function matchesPath(path, basePath) {
  return path === basePath || path.startsWith(`${basePath}/`);
}

export function getActiveAdminSection(path) {
  if (
    path === "/admin" ||
    path === "/admin/" ||
    matchesPath(path, "/admin/accounts")
  ) {
    return "accounts";
  }

  return (
    sectionBasePaths.find(([, basePath]) => matchesPath(path, basePath))?.[0] || ""
  );
}
