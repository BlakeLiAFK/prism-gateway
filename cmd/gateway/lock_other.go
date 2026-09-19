//go:build !linux && !darwin && !freebsd

package main

import "errors"

func lockDatabase(path string) (func(), error) {
	return nil, errors.New("此离线版本正式支持 macOS / Linux；Windows 请在 WSL2 中构建运行")
}
