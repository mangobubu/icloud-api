const sectionBasePaths = [
  ["aliases", "/aliases"],
  ["audit", "/audit"],
  ["logs", "/logs"],
  ["security", "/security"],
];

function matchesPath(path, basePath) {
  return path === basePath || path.startsWith(`${basePath}/`);
}

export function getActiveAdminSection(path) {
  if (
    path === "" ||
    path === "/" ||
    matchesPath(path, "/accounts")
  ) {
    return "accounts";
  }

  return (
    sectionBasePaths.find(([, basePath]) => matchesPath(path, basePath))?.[0] || ""
  );
}
