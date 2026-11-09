//go:build helper

package main

import "fmt"

func helperData(target string) ([]byte, error) {
	return nil, fmt.Errorf("this is a remote helper build")
}
