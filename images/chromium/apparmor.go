package chromium

// The Umbrel app's hooks/pre-start carries copies of apparmor/browserpier
// and apparmor/load, since Umbrel's app update copies only the files it
// knows. After editing either, refresh them with
// `go generate -run gen_umbrel_hook ./images/chromium`.
//
//go:generate go run gen_umbrel_hook.go
