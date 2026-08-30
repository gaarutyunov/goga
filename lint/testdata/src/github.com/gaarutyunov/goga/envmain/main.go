// Package main is the clean fixture for the exemption that is the rule rather
// than a hole in it. A command is where the process meets its environment: it
// has to read the variable that says where the config file is before there is
// any configuration to read it from. Every other package runs after Load has
// finished, which is when a direct read starts overriding what already
// resolved.
//
// The directory is deliberately NOT called cmd/. The exemption is keyed on the
// package name, because where a command's directory lives is a layout choice
// and this rule is about a package's position in the program.
package main

import "os"

func main() {
	path := os.Getenv("GOGA_CONFIG")
	if _, ok := os.LookupEnv("GOGA_PROFILE"); ok {
		_ = os.Environ()
	}
	_ = path
}
