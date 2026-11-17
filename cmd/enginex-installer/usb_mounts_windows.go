package main

import "golang.org/x/sys/windows"

func usbMountPaths() ([]string, error) {
	drives, err := windows.GetLogicalDrives()
	if err != nil {
		return nil, err
	}
	var paths []string
	for letter := byte('A'); letter <= 'Z'; letter++ {
		if drives&(1<<(letter-'A')) != 0 {
			paths = append(paths, string(letter)+`:\`)
		}
	}
	return paths, nil
}
