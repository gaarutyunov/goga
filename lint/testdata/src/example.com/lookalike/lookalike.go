// Package lookalike exports a String(key, value) function with the same shape
// as attribute.String, from a different import path. It exists so the clean
// fixture can prove gogasemconv keys on the import path and not on the
// identifier or the function name.
package lookalike

// String joins a key and a value.
func String(k, v string) string { return k + "=" + v }
