module github.com/ZacxDev/promptshipper/server/clip

go 1.26.0

// 🔴 THE ONLY NON-STDLIB DEPENDENCY, AND IT MUST BE PURE GO. The Dockerfile
// builds CGO_ENABLED=0 onto gcr.io/distroless/static-debian12:nonroot, so a
// cgo-backed driver (github.com/mattn/go-sqlite3, the usual first choice) would
// neither link nor run here. modernc.org/sqlite is sqlite3.c transpiled to Go,
// and its generated build carries -DSQLITE_ENABLE_FTS5 (read out of
// lib/sqlite_linux_amd64.go's generator line, which this service's `q` parameter
// depends on absolutely — without FTS5 the virtual table is a startup error).
//
require modernc.org/sqlite v1.51.0

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/sys v0.42.0 // indirect
	modernc.org/libc v1.72.3 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)
