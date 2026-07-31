//go:build !unix

package artifact

import "os"

func openArtifactNoFollow(path string) (*os.File, error) {
	return os.Open(path)
}

func syncArtifactDirectory(string) error {
	return nil
}
