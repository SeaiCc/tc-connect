package harness

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestNewConcurrentController(t *testing.T) {
	tests := []struct {
		name          string
		cfg           *ConcurrentControllerConfig
		expectGlobal  int
		expectProject int
	}{
		{
			name: "default config",
			cfg:  nil,
			expectGlobal:  5,
			expectProject: 2,
		},
		{
			name: "custom config",
			cfg: &ConcurrentControllerConfig{
				MaxGlobal:     10,
				MaxPerProject: 3,
			},
			expectGlobal:  10,
			expectProject: 3,
		},
		{
			name: "zero values use default",
			cfg: &ConcurrentControllerConfig{
				MaxGlobal:     0,
				MaxPerProject: 0,
			},
			expectGlobal:  5,
			expectProject: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := NewConcurrentController(tt.cfg)
			if ctrl == nil {
				t.Fatal("controller is nil")
			}

			// 尝试获取许可验证容量
			// 这里只是简单验证控制器创建成功
			ctrl.Close()
		})
	}
}

func TestConcurrentController_AcquireRelease(t *testing.T) {
	ctrl := NewConcurrentController(&ConcurrentControllerConfig{
		MaxGlobal:     2,
		MaxPerProject: 1,
	})
	defer ctrl.Close()

	// 测试获取和释放
	ctx := context.Background()

	// 第一个任务应该成功
	err := ctrl.Acquire(ctx, "project1", "agent1", 5*time.Second)
	if err != nil {
		t.Errorf("first acquire failed: %v", err)
	}

	// 验证活跃数
	if active := ctrl.GetActiveCount(); active != 1 {
		t.Errorf("expected active count 1, got %d", active)
	}

	// 释放
	ctrl.Release("project1")

	// 验证活跃数为 0
	if active := ctrl.GetActiveCount(); active != 0 {
		t.Errorf("expected active count 0 after release, got %d", active)
	}
}

func TestConcurrentController_GlobalLimit(t *testing.T) {
	ctrl := NewConcurrentController(&ConcurrentControllerConfig{
		MaxGlobal:     2,
		MaxPerProject: 2,
	})
	defer ctrl.Close()

	ctx := context.Background()

	// 获取 2 个许可（达到上限）
	if err := ctrl.Acquire(ctx, "project1", "agent1", 100*time.Millisecond); err != nil {
		t.Fatalf("first acquire failed: %v", err)
	}

	if err := ctrl.Acquire(ctx, "project2", "agent2", 100*time.Millisecond); err != nil {
		t.Fatalf("second acquire failed: %v", err)
	}

	// 第三个应该失败（超时）
	err := ctrl.Acquire(ctx, "project3", "agent3", 100*time.Millisecond)
	if err == nil {
		t.Error("expected timeout error for third acquire, got nil")
	} else if ctrl.GetActiveCount() != 2 {
		t.Errorf("expected active count 2, got %d", ctrl.GetActiveCount())
	}

	// 清理
	ctrl.Release("project1")
	ctrl.Release("project2")
}

func TestConcurrentController_ProjectIsolation(t *testing.T) {
	ctrl := NewConcurrentController(&ConcurrentControllerConfig{
		MaxGlobal:     5,
		MaxPerProject: 1, // 每项目只能 1 个
	})
	defer ctrl.Close()

	ctx := context.Background()

	// project1 获取 1 个
	if err := ctrl.Acquire(ctx, "project1", "agent1", 100*time.Millisecond); err != nil {
		t.Fatalf("project1 first acquire failed: %v", err)
	}

	// project2 获取 1 个（应该成功，因为是不同项目）
	if err := ctrl.Acquire(ctx, "project2", "agent1", 100*time.Millisecond); err != nil {
		t.Fatalf("project2 acquire failed: %v", err)
	}

	// project1 再获取 1 个（应该失败，因为 project1 已经达到每项目限制）
	err := ctrl.Acquire(ctx, "project1", "agent2", 100*time.Millisecond)
	if err == nil {
		t.Error("expected error for project1 second acquire (project limit), got nil")
	}

	// 验证活跃项目
	activeProjects := ctrl.GetActiveProjects()
	if len(activeProjects) != 2 {
		t.Errorf("expected 2 active projects, got %d: %v", len(activeProjects), activeProjects)
	}

	// 清理
	ctrl.Release("project1")
	ctrl.Release("project2")
}

func TestConcurrentController_ConcurrentAccess(t *testing.T) {
	ctrl := NewConcurrentController(&ConcurrentControllerConfig{
		MaxGlobal:     3,
		MaxPerProject: 3,
	})
	defer ctrl.Close()

	ctx := context.Background()
	var wg sync.WaitGroup
	var mu sync.Mutex
	maxActiveSeen := 0
	totalCompleted := 0

	// 启动 10 个 goroutine 竞争 3 个许可
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			projectName := "project1"
			if err := ctrl.Acquire(ctx, projectName, "agent1", 5*time.Second); err != nil {
				// 超时不应该发生
				t.Errorf("goroutine %d acquire failed: %v", id, err)
				return
			}

			mu.Lock()
			currentActive := ctrl.GetActiveCount()
			if currentActive > maxActiveSeen {
				maxActiveSeen = currentActive
			}
			totalCompleted++
			mu.Unlock()

			// 验证并发数不超过限制
			if currentActive > 3 {
				t.Errorf("active count %d exceeds max 3", currentActive)
			}

			// 模拟工作
			time.Sleep(10 * time.Millisecond)

			ctrl.Release(projectName)
		}(i)
	}

	wg.Wait()

	// 所有 10 个都应该完成（串行化）
	if totalCompleted != 10 {
		t.Errorf("expected 10 completed, got %d", totalCompleted)
	}

	// 最大活跃数应该不超过 3
	if maxActiveSeen > 3 {
		t.Errorf("max active seen %d exceeds limit 3", maxActiveSeen)
	}

	// 最终活跃数应该为 0
	if active := ctrl.GetActiveCount(); active != 0 {
		t.Errorf("expected final active count 0, got %d", active)
	}
}

func TestConcurrentController_IsAvailable(t *testing.T) {
	ctrl := NewConcurrentController(&ConcurrentControllerConfig{
		MaxGlobal:     2,
		MaxPerProject: 2,
	})
	defer ctrl.Close()

	ctx := context.Background()

	// 初始应该有许可
	if !ctrl.IsAvailable("project1") {
		t.Error("expected available initially")
	}

	// 获取 2 个许可
	if err := ctrl.Acquire(ctx, "project1", "agent1", 100*time.Millisecond); err != nil {
		t.Fatalf("first acquire failed: %v", err)
	}
	if err := ctrl.Acquire(ctx, "project2", "agent1", 100*time.Millisecond); err != nil {
		t.Fatalf("second acquire failed: %v", err)
	}

	// 现在不应该有许可了
	if ctrl.IsAvailable("project3") {
		t.Error("expected not available after filling global pool")
	}

	// 释放一个
	ctrl.Release("project1")

	// 现在应该有许可了
	if !ctrl.IsAvailable("project3") {
		t.Error("expected available after release")
	}

	// 清理
	ctrl.Release("project2")
	// 注意：IsAvailable 会临时获取再释放，所以不需要额外释放 project3
}

func TestConcurrentController_GetStats(t *testing.T) {
	ctrl := NewConcurrentController(&ConcurrentControllerConfig{
		MaxGlobal:     5,
		MaxPerProject: 2,
	})
	defer ctrl.Close()

	ctx := context.Background()

	// 获取一些许可
	ctrl.Acquire(ctx, "project1", "agent1", 100*time.Millisecond)
	ctrl.Acquire(ctx, "project1", "agent2", 100*time.Millisecond)
	ctrl.Acquire(ctx, "project2", "agent1", 100*time.Millisecond)

	stats := ctrl.GetStats()

	// 验证统计信息
	if active, ok := stats["active_global"].(int); !ok || active != 3 {
		t.Errorf("expected active_global 3, got %v", stats["active_global"])
	}

	if max, ok := stats["max_global"].(int); !ok || max != 5 {
		t.Errorf("expected max_global 5, got %v", stats["max_global"])
	}

	// 验证利用率
	if util, ok := stats["utilization"].(float64); !ok || util != 0.6 {
		t.Errorf("expected utilization 0.6, got %v", stats["utilization"])
	}

	// 清理
	ctrl.Release("project1")
	ctrl.Release("project1")
	ctrl.Release("project2")
}

func TestConcurrentController_Cleanup(t *testing.T) {
	ctrl := NewConcurrentController(&ConcurrentControllerConfig{
		MaxGlobal:     5,
		MaxPerProject: 2,
	})
	defer ctrl.Close()

	ctx := context.Background()

	// 创建一些项目池
	ctrl.Acquire(ctx, "project1", "agent1", 100*time.Millisecond)
	ctrl.Release("project1")

	ctrl.Acquire(ctx, "project2", "agent1", 100*time.Millisecond)
	ctrl.Release("project2")

	// 清理（应该移除 2 个空闲项目池）
	removed := ctrl.Cleanup(0) // 0 表示使用默认阈值
	if removed != 2 {
		t.Errorf("expected 2 removed, got %d", removed)
	}
}

func TestConcurrentController_ContextCancellation(t *testing.T) {
	ctrl := NewConcurrentController(&ConcurrentControllerConfig{
		MaxGlobal:     1,
		MaxPerProject: 1,
	})
	defer ctrl.Close()

	// 先占满
	ctx1 := context.Background()
	if err := ctrl.Acquire(ctx1, "project1", "agent1", 100*time.Millisecond); err != nil {
		t.Fatalf("failed to acquire first lock: %v", err)
	}

	// 创建可取消的上下文
	ctx2, cancel := context.WithCancel(context.Background())

	// 尝试获取（应该阻塞）
	done := make(chan error, 1)
	go func() {
		done <- ctrl.Acquire(ctx2, "project2", "agent1", 5*time.Second)
	}()

	// 稍后取消
	time.Sleep(50 * time.Millisecond)
	cancel()

	// 应该收到取消错误
	err := <-done
	if err == nil {
		t.Error("expected context cancelled error")
	} else if !contains(err.Error(), "context") {
		t.Errorf("expected context error, got %v", err)
	}

	// 清理
	ctrl.Release("project1")
}

// contains 辅助函数
func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestConcurrentController_Reset(t *testing.T) {
	ctrl := NewConcurrentController(&ConcurrentControllerConfig{
		MaxGlobal:     2,
		MaxPerProject: 2,
	})

	ctx := context.Background()

	// 占用一些资源
	ctrl.Acquire(ctx, "project1", "agent1", 100*time.Millisecond)
	ctrl.Acquire(ctx, "project2", "agent1", 100*time.Millisecond)

	if ctrl.GetActiveCount() != 2 {
		t.Errorf("expected active count 2 before reset, got %d", ctrl.GetActiveCount())
	}

	// 重置
	ctrl.Reset()

	if ctrl.GetActiveCount() != 0 {
		t.Errorf("expected active count 0 after reset, got %d", ctrl.GetActiveCount())
	}

	// 现在应该可以获取了
	if err := ctrl.Acquire(ctx, "project3", "agent1", 100*time.Millisecond); err != nil {
		t.Errorf("acquire after reset failed: %v", err)
	}
	ctrl.Release("project3")

	ctrl.Close()
}

func BenchmarkConcurrentController_AcquireRelease(b *testing.B) {
	ctrl := NewConcurrentController(&ConcurrentControllerConfig{
		MaxGlobal:     100,
		MaxPerProject: 100,
	})
	defer ctrl.Close()

	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ctrl.Acquire(ctx, "project1", "agent1", 0)
		ctrl.Release("project1")
	}
}

func BenchmarkConcurrentController_Concurrent(b *testing.B) {
	ctrl := NewConcurrentController(&ConcurrentControllerConfig{
		MaxGlobal:     10,
		MaxPerProject: 10,
	})
	defer ctrl.Close()

	ctx := context.Background()
	var wg sync.WaitGroup

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := ctrl.Acquire(ctx, "project1", "agent1", 100*time.Millisecond); err == nil {
					ctrl.Release("project1")
				}
			}()
		}
	})
	wg.Wait()
}

func TestConcurrentController_EmptyProjectName(t *testing.T) {
	ctrl := NewConcurrentController(&ConcurrentControllerConfig{
		MaxGlobal:     2,
		MaxPerProject: 2,
	})
	defer ctrl.Close()

	ctx := context.Background()

	// 空字符串项目名应该被映射为 default
	if err := ctrl.Acquire(ctx, "", "agent1", 100*time.Millisecond); err != nil {
		t.Fatalf("acquire with empty project name failed: %v", err)
	}

	if err := ctrl.Acquire(ctx, "", "agent2", 100*time.Millisecond); err != nil {
		t.Fatalf("second acquire with empty project name failed: %v", err)
	}

	// 第三个应该失败（达到每项目限制）
	err := ctrl.Acquire(ctx, "", "agent3", 100*time.Millisecond)
	if err == nil {
		t.Error("expected error for third acquire with empty project name (project limit), got nil")
	}

	// 清理
	ctrl.Release("")
	ctrl.Release("")
}

func TestConcurrentController_ExtremeValues(t *testing.T) {
	tests := []struct {
		name          string
		maxGlobal     int
		maxPerProject int
		shouldWork    bool
	}{
		{"zero values use defaults", 0, 0, true},
		{"maxGlobal=1", 1, 1, true},
		{"maxGlobal=1000", 1000, 100, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := NewConcurrentController(&ConcurrentControllerConfig{
				MaxGlobal:     tt.maxGlobal,
				MaxPerProject: tt.maxPerProject,
			})
			defer ctrl.Close()

			if !tt.shouldWork {
				return
			}

			ctx := context.Background()
			// 尝试获取一个许可
			if err := ctrl.Acquire(ctx, "project1", "agent1", 100*time.Millisecond); err != nil {
				t.Errorf("acquire failed with config maxGlobal=%d, maxPerProject=%d: %v", 
					tt.maxGlobal, tt.maxPerProject, err)
			} else {
				ctrl.Release("project1")
			}
		})
	}
}

func TestConcurrentController_ManyProjectsMemory(t *testing.T) {
	ctrl := NewConcurrentController(&ConcurrentControllerConfig{
		MaxGlobal:     100,
		MaxPerProject: 10,
	})
	defer ctrl.Close()

	ctx := context.Background()

	// 创建 100 个不同的项目
	const numProjects = 100
	for i := 0; i < numProjects; i++ {
		projectName := fmt.Sprintf("project_%d", i)
		if err := ctrl.Acquire(ctx, projectName, "agent1", 100*time.Millisecond); err != nil {
			t.Fatalf("acquire for project %d failed: %v", i, err)
		}
		ctrl.Release(projectName)
	}

	// 清理所有空闲项目
	removed := ctrl.Cleanup(0)
	if removed != numProjects {
		t.Errorf("expected to remove %d projects, got %d", numProjects, removed)
	}

	// 验证活跃项目数为 0
	if active := len(ctrl.GetActiveProjects()); active != 0 {
		t.Errorf("expected 0 active projects after cleanup, got %d", active)
	}
}

func TestConcurrentController_RaceConditionInStats(t *testing.T) {
	ctrl := NewConcurrentController(&ConcurrentControllerConfig{
		MaxGlobal:     3,
		MaxPerProject: 3,
	})
	defer ctrl.Close()

	ctx := context.Background()
	var wg sync.WaitGroup

	// 大量并发读取统计信息
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = ctrl.GetStats()
			_ = ctrl.GetActiveCount()
			_ = ctrl.GetActiveProjects()
			_ = ctrl.IsAvailable("project1")
		}()
	}

	// 同时写入
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			if err := ctrl.Acquire(ctx, "project1", "agent1", 5*time.Second); err == nil {
				time.Sleep(time.Millisecond)
				ctrl.Release("project1")
			}
		}(i)
	}

	wg.Wait()
}
