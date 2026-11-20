package harnesstest

// Absolute binary paths for the install tests. Bin has no spaces, so cmd.exe
// and PowerShell commands leave it bare and the golden files match once
// GoldenBin stands in for it. Quoting of paths with spaces is tested in each
// harness package.
const (
	Bin      = `C:\Logbook\bin\logbook.exe`
	OtherBin = `C:\Users\Teen Dev\AppData\Local\Programs\Logbook\logbook.exe`
)
