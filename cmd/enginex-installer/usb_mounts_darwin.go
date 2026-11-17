package main

import (
	"os"
	"path/filepath"
)

func usbMountPaths() ([]string, error) {
	volumes, err := os.ReadDir("/Volumes")
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, volume := range volumes {
		if volume.IsDir() {
			paths = append(paths, filepath.Join("/Volumes", volume.Name()))
		}
	}
	return paths, nil
}
