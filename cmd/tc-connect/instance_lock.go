package main

import (
	"fmt"
	"os"
	"path/filepath"
)

type InstanceLock struct {
	path string
}

// 获取一个锁用于给定的配置文件, windows上总是成功(空操作)
func AcquireInstanceLock(configPath string) (*InstanceLock, error) {
	configDir := filepath.Dir(configPath)
	configBase := filepath.Base(configPath)
	lockName := fmt.Sprintf(".%s.lock", configBase)
	lockPath := filepath.Join(configDir, lockName)

	// 将PID写入lockfile 用于诊断
	pid := os.Getpid()
	// Non-fatal on Windows
	_ = os.WriteFile(lockPath, []byte(fmt.Sprintf("%d\n", pid)), 0644)

	return &InstanceLock{path: lockPath}, nil
}

func (l *InstanceLock) Path() string {
	return l.path
}
