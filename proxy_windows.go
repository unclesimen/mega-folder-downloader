//go:build windows

package main

import (
	"os/exec"
	"strings"
)

// systemProxy 读取 Windows 系统代理（设置 → 网络和 Internet → 代理，
// 也就是浏览器在用的那套，注册表 Internet Settings）。未启用时返回空。
func systemProxy() string {
	if regQuery("ProxyEnable") != "0x1" {
		return ""
	}
	return regQuery("ProxyServer")
}

func regQuery(value string) string {
	out, err := exec.Command("reg", "query",
		`HKCU\Software\Microsoft\Windows\CurrentVersion\Internet Settings`,
		"/v", value).Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, value) {
			fields := strings.Fields(line)
			if len(fields) >= 3 {
				return fields[len(fields)-1]
			}
		}
	}
	return ""
}
