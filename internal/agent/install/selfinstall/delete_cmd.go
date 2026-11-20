package selfinstall

// deleteCommand returns the cmd.exe command line that deletes path once it
// is no longer running, retrying for about 30 seconds, then removes dir if
// it is empty. It is plain text so it can be tested on any OS. Windows
// paths cannot contain '"', so quoting them is enough.
func deleteCommand(path, dir string) string {
	return `cmd.exe /d /q /c "for /l %i in (1,1,30) do @(del /f /q "` + path + `" 2>nul & if exist "` + path +
		`" ping -n 2 127.0.0.1 >nul) & rmdir "` + dir + `" 2>nul"`
}
