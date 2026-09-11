package main

import (
	"os"
	"path/filepath"
	"time"

	"huawei.com/devbridge/cmd"
	"huawei.com/devbridge/internal/updater"
)

func main() {
	cmd.RootCmd.Use = filepath.Base(os.Args[0])
	err := cmd.RootCmd.Execute()
	// 给异步版本检查一个短暂的存活窗口：快速命令（如 list/show）执行完时，
	// 后台 goroutine 可能刚发起联网请求，若进程立即退出则提示丢失。这里最多
	// 等 1 秒让它跑完；命令本身耗时长的场景，此时 goroutine 通常已自然完成。
	updater.WaitAsync(time.Second)
	if err != nil {
		os.Exit(1)
	}
}
