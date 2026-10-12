//go:build !windows

package main

// 非 Windows 平台不读取系统代理（可用 -proxy 参数或环境变量指定）。
func systemProxy() string { return "" }
