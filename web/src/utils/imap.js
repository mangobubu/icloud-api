export const DEFAULT_IMAP_HOST = "imap.mail.me.com";
export const DEFAULT_IMAP_PORT = 993;
export const MIN_IMAP_PORT = 1;
export const MAX_IMAP_PORT = 65535;

// Match Go's strings.TrimSpace/unicode.IsSpace contract. JavaScript's trim()
// differs for U+0085 and U+FEFF, which could make client validation disagree
// with NormalizeIMAPEndpoint after a pasted host value.
const GO_SPACE =
  "\\u0009-\\u000d\\u0020\\u0085\\u00a0\\u1680\\u2000-\\u200a\\u2028\\u2029\\u202f\\u205f\\u3000";
const GO_SPACE_AT_EDGES = new RegExp(`^[${GO_SPACE}]+|[${GO_SPACE}]+$`, "g");
const GO_SPACE_ANYWHERE = new RegExp(`[${GO_SPACE}]`);

function normalizedString(value) {
  return typeof value === "string"
    ? value.replace(GO_SPACE_AT_EDGES, "")
    : "";
}

function numericPort(value) {
  if (typeof value === "number") return value;
  if (typeof value === "string" && /^\d+$/.test(value.trim())) {
    return Number(value.trim());
  }
  return Number.NaN;
}

function canonicalIPv4(host) {
  const labels = host.split(".");
  if (labels.length !== 4 || labels.some((label) => !/^\d+$/.test(label))) {
    return null;
  }
  const octets = labels.map((label) => Number(label));
  if (
    octets.some(
      (octet, index) =>
        !Number.isInteger(octet) ||
        octet < 0 ||
        octet > 255 ||
        String(octet) !== labels[index],
    )
  ) {
    return null;
  }
  return octets.join(".");
}

function canonicalIPv6(host) {
  // URL parsing gives us the same compressed, lower-case representation in
  // browsers that net.ParseIP/String gives the server.
  if (!host.includes(":")) return null;
  try {
    const parsed = new URL(`http://[${host}]/`);
    const canonical = parsed.hostname;
    if (!canonical.startsWith("[") || !canonical.endsWith("]")) return null;
    const unbracketed = canonical.slice(1, -1).toLowerCase();
    const mappedIPv4 = /^::ffff:([0-9a-f]{1,4}):([0-9a-f]{1,4})$/i.exec(
      unbracketed,
    );
    if (mappedIPv4) {
      const high = Number.parseInt(mappedIPv4[1], 16);
      const low = Number.parseInt(mappedIPv4[2], 16);
      return [high >>> 8, high & 0xff, low >>> 8, low & 0xff].join(".");
    }
    return unbracketed;
  } catch {
    return null;
  }
}

function canonicalHost(value) {
  const host = normalizedString(value);
  if (!host || host.length > 253 || GO_SPACE_ANYWHERE.test(host)) return null;
  if (host.includes("[") || host.includes("]")) return null;

  const ipv4 = canonicalIPv4(host);
  if (ipv4) return ipv4;
  const ipv6 = canonicalIPv6(host);
  if (ipv6) return ipv6;

  const withoutRoot = host.endsWith(".") ? host.slice(0, -1) : host;
  if (!withoutRoot || withoutRoot.length > 253) return null;
  const labels = withoutRoot.split(".");
  if (
    labels.some(
      (label) =>
        label.length < 1 ||
        label.length > 63 ||
        label.startsWith("-") ||
        label.endsWith("-") ||
        !/^[A-Za-z0-9-]+$/.test(label),
    )
  ) {
    return null;
  }
  return withoutRoot.toLowerCase();
}

/** Return a canonical host, falling back to the iCloud host for bad/missing data. */
export function normalizeIMAPHost(value, fallback = DEFAULT_IMAP_HOST) {
  return canonicalHost(value) || canonicalHost(fallback) || DEFAULT_IMAP_HOST;
}

/** Return an integer TCP port, falling back to the default for bad/missing data. */
export function normalizeIMAPPort(value, fallback = DEFAULT_IMAP_PORT) {
  const number = numericPort(value);
  if (
    Number.isInteger(number) &&
    number >= MIN_IMAP_PORT &&
    number <= MAX_IMAP_PORT
  ) {
    return number;
  }
  const fallbackNumber = numericPort(fallback);
  return Number.isInteger(fallbackNumber) &&
    fallbackNumber >= MIN_IMAP_PORT &&
    fallbackNumber <= MAX_IMAP_PORT
    ? fallbackNumber
    : DEFAULT_IMAP_PORT;
}

/** Normalize endpoint fields into the stable shape used by the admin UI. */
export function normalizeIMAPEndpoint(host, port) {
  return {
    host: normalizeIMAPHost(host),
    port: normalizeIMAPPort(port),
  };
}

/** Return a Chinese validation message, or null when the host is valid. */
export function validateIMAPHost(value) {
  const normalized = normalizedString(value);
  if (!normalized) return "请填写 IMAP 主机";
  if (!canonicalHost(normalized)) return "IMAP 主机格式不正确";
  return null;
}

/** Return a Chinese validation message, or null when the port is valid. */
export function validateIMAPPort(value) {
  const number = numericPort(value);
  if (
    !Number.isInteger(number) ||
    number < MIN_IMAP_PORT ||
    number > MAX_IMAP_PORT
  ) {
    return `IMAP 端口应为 ${MIN_IMAP_PORT}-${MAX_IMAP_PORT} 的整数`;
  }
  return null;
}

export function isValidIMAPHost(value) {
  return validateIMAPHost(value) === null;
}

export function isValidIMAPPort(value) {
  return validateIMAPPort(value) === null;
}

/** Format an endpoint with brackets where required by an IPv6 host. */
export function formatIMAPEndpoint(hostOrEndpoint, port) {
  const input =
    hostOrEndpoint && typeof hostOrEndpoint === "object"
      ? hostOrEndpoint
      : { host: hostOrEndpoint, port };
  const endpoint = normalizeIMAPEndpoint(
    input.host ?? input.imapHost ?? input.imap_host,
    input.port ?? input.imapPort ?? input.imap_port,
  );
  const displayHost = endpoint.host.includes(":")
    ? `[${endpoint.host}]`
    : endpoint.host;
  return `${displayHost}:${endpoint.port}`;
}

// Keep a camel-case alias convenient for callers that use Imap in identifiers.
export const formatImapEndpoint = formatIMAPEndpoint;
