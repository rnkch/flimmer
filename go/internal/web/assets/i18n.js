// Translations for the browser scripts. The server renders the strings for
// the current language into <body data-i18n> (see i18n.JS in Go).
let table;

export function t(key, ...args) {
  table ??= JSON.parse(document.body.dataset.i18n || '{}');
  return (table[key] ?? key).replace(/\{(\d+)\}/g, (_, i) => args[i]);
}

export function files(n) {
  return t(n === 1 ? '{0} file' : '{0} files', n);
}
