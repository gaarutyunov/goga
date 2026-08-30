package bypass

import nethttp "net/http"

// Aliased proves the rule keys on the import PATH and not on the identifier
// "http". Renaming the import is not a fix, and a rule that could be silenced
// by an alias would be a rule about spelling.
func Aliased(h nethttp.Handler) error {
	_ = &nethttp.Server{Handler: h} // want `constructing an http\.Server directly bypasses serve\.New`

	return nethttp.ListenAndServe(":8080", h) // want `serving HTTP through http\.ListenAndServe bypasses serve\.New`
}
