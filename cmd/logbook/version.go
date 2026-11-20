package main

// version is set at release time with -ldflags "-X main.version=<tag>". It
// is the one package-level variable here, because the linker can only set
// variables.
var version = "dev"
