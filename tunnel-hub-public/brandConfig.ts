import { readFileSync } from 'node:fs';
import { parseAllDocuments } from 'yaml';

export type BrandConfig = {
  schemaVersion: 1;
  brand: {
    id: string;
    productName: string;
    publicSiteTitle: string;
  };
  domains: {
    publicBase: string;
    desktopPublicBase: string;
    webAppPublicBase: string;
  };
  endpoints: {
    relayPublicUrl: string;
    sharePublicBaseUrl: string;
  };
};

const brandIDPattern = /^[a-z][a-z0-9-]*$/;
const hostLabelPattern = /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/;

export function loadBrandConfig(path: string): BrandConfig {
  const documents = parseAllDocuments(readFileSync(path, 'utf8'), { strict: true });
  if (documents.length !== 1) {
    throw new Error(`brand config ${path} must contain exactly one YAML document`);
  }
  const document = documents[0];
  if (document.errors.length > 0) {
    throw new Error(`decode brand config ${path}: ${document.errors.map((error) => error.message).join('; ')}`);
  }
  const raw = document.toJS() as unknown;
  const root = strictRecord(raw, 'brand config', ['schemaVersion', 'brand', 'domains', 'endpoints']);
  if (root.schemaVersion !== 1) {
    throw new Error('schemaVersion must be 1');
  }

  const brand = strictRecord(root.brand, 'brand', ['id', 'productName', 'publicSiteTitle']);
  const domains = strictRecord(root.domains, 'domains', ['publicBase', 'desktopPublicBase', 'webAppPublicBase']);
  const endpoints = strictRecord(root.endpoints, 'endpoints', ['relayPublicUrl', 'sharePublicBaseUrl']);
  const id = requiredString(brand.id, 'brand.id');
  if (!brandIDPattern.test(id)) {
    throw new Error('brand.id must start with a lowercase letter and contain only lowercase letters, digits, and hyphens');
  }

  const normalizedDomains = {
    publicBase: hostname(domains.publicBase, 'domains.publicBase'),
    desktopPublicBase: hostname(domains.desktopPublicBase, 'domains.desktopPublicBase'),
    webAppPublicBase: hostname(domains.webAppPublicBase, 'domains.webAppPublicBase')
  };
  if (new Set(Object.values(normalizedDomains)).size !== 3) {
    throw new Error('the three domains must be different');
  }

  const relayPublicUrl = stringValue(endpoints.relayPublicUrl, 'endpoints.relayPublicUrl');
  const sharePublicBaseUrl = stringValue(endpoints.sharePublicBaseUrl, 'endpoints.sharePublicBaseUrl');
  if (relayPublicUrl) {
    validateRelayURL(relayPublicUrl);
  }
  if (sharePublicBaseUrl) {
    validateShareOrigin(sharePublicBaseUrl);
  }

  return {
    schemaVersion: 1,
    brand: {
      id,
      productName: requiredString(brand.productName, 'brand.productName'),
      publicSiteTitle: requiredString(brand.publicSiteTitle, 'brand.publicSiteTitle')
    },
    domains: normalizedDomains,
    endpoints: { relayPublicUrl, sharePublicBaseUrl }
  };
}

export function injectPublicSiteTitle(html: string, title: string) {
  const escaped = title
    .replaceAll('&', '&amp;')
    .replaceAll('<', '&lt;')
    .replaceAll('>', '&gt;')
    .replaceAll('"', '&quot;')
    .replaceAll("'", '&#39;');
  if (!/<title>[\s\S]*?<\/title>/i.test(html)) {
    throw new Error('index.html must contain a title element');
  }
  return html.replace(/<title>[\s\S]*?<\/title>/i, `<title>${escaped}</title>`);
}

function strictRecord(value: unknown, field: string, allowedKeys: string[]): Record<string, unknown> {
  if (!value || typeof value !== 'object' || Array.isArray(value)) {
    throw new Error(`${field} must be an object`);
  }
  const record = value as Record<string, unknown>;
  for (const key of Object.keys(record)) {
    if (!allowedKeys.includes(key)) {
      throw new Error(`${field} contains unknown field ${key}`);
    }
  }
  for (const key of allowedKeys) {
    if (!(key in record)) {
      throw new Error(`${field}.${key} is required`);
    }
  }
  return record;
}

function stringValue(value: unknown, field: string) {
  if (typeof value !== 'string') {
    throw new Error(`${field} must be a string`);
  }
  return value;
}

function requiredString(value: unknown, field: string) {
  const text = stringValue(value, field);
  if (!text.trim()) {
    throw new Error(`${field} must not be empty`);
  }
  return text;
}

function hostname(value: unknown, field: string) {
  const text = requiredString(value, field).trim().toLowerCase();
  if (
    text.length > 253 ||
    /[:/?#*@]/.test(text) ||
    text.startsWith('.') ||
    text.endsWith('.') ||
    isIPAddress(text) ||
    text.split('.').some((label) => !hostLabelPattern.test(label))
  ) {
    throw new Error(`${field} must be a hostname without scheme, port, path, wildcard, or IP address`);
  }
  return text;
}

function validateRelayURL(value: string) {
  const url = parsedURL(value, 'endpoints.relayPublicUrl');
  validateEndpointHostname(url.hostname, 'endpoints.relayPublicUrl');
  if (url.username || url.password || url.search || url.hash || url.pathname !== '/tunnel') {
    throw new Error('endpoints.relayPublicUrl must have path /tunnel and no credentials, query, or fragment');
  }
  if (isLoopbackHost(url.hostname)) {
    if (url.protocol !== 'ws:' && url.protocol !== 'wss:') {
      throw new Error('loopback endpoints.relayPublicUrl must use ws or wss');
    }
  } else if (url.protocol !== 'wss:') {
    throw new Error('non-loopback endpoints.relayPublicUrl must use wss');
  }
}

function validateShareOrigin(value: string) {
  const url = parsedURL(value, 'endpoints.sharePublicBaseUrl');
  validateEndpointHostname(url.hostname, 'endpoints.sharePublicBaseUrl');
  if (url.username || url.password || url.search || url.hash || (url.pathname !== '' && url.pathname !== '/')) {
    throw new Error('endpoints.sharePublicBaseUrl must be an origin without credentials, path, query, or fragment');
  }
  if (isLoopbackHost(url.hostname)) {
    if (url.protocol !== 'http:' && url.protocol !== 'https:') {
      throw new Error('loopback endpoints.sharePublicBaseUrl must use http or https');
    }
  } else if (url.protocol !== 'https:') {
    throw new Error('non-loopback endpoints.sharePublicBaseUrl must use https');
  }
}

function parsedURL(value: string, field: string) {
  try {
    const url = new URL(value);
    if (!url.hostname) {
      throw new Error('missing hostname');
    }
    return url;
  } catch {
    throw new Error(`${field} must be a valid URL`);
  }
}

function isLoopbackHost(host: string) {
  const normalized = host.toLowerCase().replace(/^\[|\]$/g, '');
  return normalized === 'localhost' || normalized === '127.0.0.1' || normalized === '::1';
}

function validateEndpointHostname(host: string, field: string) {
  const normalized = host.toLowerCase().replace(/^\[|\]$/g, '');
  if (isForbiddenEndpointHost(normalized)) {
    throw new Error(`${field} must not use a reserved non-canonical local hostname`);
  }
  if (isIPAddress(normalized)) {
    return;
  }
  if (normalized.length > 253 || normalized.startsWith('.') || normalized.endsWith('.') || normalized.split('.').some((label) => !hostLabelPattern.test(label))) {
    throw new Error(`${field} must use a valid hostname`);
  }
}

function isForbiddenEndpointHost(host: string) {
  if (isLoopbackHost(host)) {
    return false;
  }
  return host === '0.0.0.0' || host.endsWith('.localhost') || /^127(?:\.\d{1,3}){3}$/.test(host);
}

function isIPAddress(host: string) {
  return /^\d{1,3}(?:\.\d{1,3}){3}$/.test(host) || host.includes(':');
}
