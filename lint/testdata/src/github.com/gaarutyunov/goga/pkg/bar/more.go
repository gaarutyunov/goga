package bar // want `package .* lives inside "pkg/"; goga.s own code is laid out flat`

// More proves the diagnostic is reported once per file, not once per package:
// every file in the offending directory has to move.
func More() string { return "more" }
