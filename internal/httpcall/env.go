package httpcall

import "os"

// osGetenv is the default env reader; tests can override `getenv` in pool.go.
func osGetenv(k string) string { return os.Getenv(k) }
