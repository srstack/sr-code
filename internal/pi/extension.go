package pi

import (
	_ "embed"
	"os"
	"path/filepath"
)

//go:embed usher-extension.ts
var usherExtension []byte

// PrepareUsherExtension materializes the embedded pi extension beside usher's
// other generated runtime configuration and returns its absolute path.
func PrepareUsherExtension(dataDir string) (string, error) {
	dir := filepath.Join(dataDir, "pi")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path, err := filepath.Abs(filepath.Join(dir, "usher-extension.ts"))
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, usherExtension, 0o600); err != nil {
		return "", err
	}
	return path, nil
}
