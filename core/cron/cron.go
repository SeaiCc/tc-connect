package cron

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"tc-connect/core/i18n"
	"tc-connect/core/types"
	"time"

	"github.com/robfig/cron/v3"
)

const defaultCronJobTimeout = 30 * time.Minute

// 表示一个持久化的定时任务
type CronJob struct {
	ID          string `json:"id"`
	Project     string `json:"project"`
	SessionKey  string `json:"session_key"`
	CronExpr    string `json:"cron_expr"`
	Prompt      string `json:"prompt"`
	Exec        string `json:"exec,omitempty"`     // shell命令; 与Prompt互斥
	WorkDir     string `json:"work_dir,omitempty"` // 用于exec的工作目录;empty = agent_work_dir
	Enabled     bool   `json:"enabled"`
	Description string `json:"description"`

	Silent      *bool  `json:"silent,omitempty"`       // 抑制启动通知; nil = 使用全局默认
	Mute        bool   `json:"mute,omitempty"`         // 抑制所有消息(启动+结果) 作业静默运行
	SessionMode string `json:"session_mode,omitempty"` // "" or "reuse" = 共享active
	Mode        string `json:"mode,omitempty"`         // "" 或 "reuse" = share active

	TimeoutMins *int      `json:"timeout_mins,omitempty"` // nil = 默认 30m wait; 0 = 无限制; >0 = minutes
	CreatedAt   time.Time `json:"created_at"`
	LastRun     time.Time `json:"last_run,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
}

// 报告是否每个cron 运行应该使用一个新的engine session 而不是重用
// 活跃的active session 作为session key
func (j *CronJob) UsesNewSessionPerRun() bool {
	return NormalizeCronSessionMode(j.SessionMode) == "new_per_run"
}

func (j *CronJob) IsShellJob() bool {
	return j.Exec != ""
}

func (j *CronJob) ExecutionTimeout() time.Duration {
	if j.TimeoutMins == nil {
		return defaultCronJobTimeout
	}
	if *j.TimeoutMins <= 0 {
		return 0
	}
	return time.Duration(*j.TimeoutMins) * time.Minute
}

// 将jobs持久化未json文件
type CronStore struct {
	path string
	mu   sync.Mutex
	jobs []*CronJob
}

func NewCronStore(dataDir string) (*CronStore, error) {
	dir := filepath.Join(dataDir, "crons")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "jobs.json")
	s := &CronStore{path: path}
	s.load()
	return s, nil
}

func (s *CronStore) load() {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	if err := json.Unmarshal(data, &s.jobs); err != nil {
		slog.Error("cron: failed to load jobs", "path", s.path, "error", err)
	}
}

func (s *CronStore) MarkRun(id string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs {
		if j.ID == id {
			j.LastRun = time.Now()
			if err != nil {
				j.LastError = err.Error()
			} else {
				j.LastError = ""
			}
		}
		if saveErr := s.save(); saveErr != nil {
			slog.Warn("cron: failed to save after mark run", "error", saveErr)
		}
		return
	}
}

func (s *CronStore) save() error {
	data, err := json.MarshalIndent(s.jobs, "", "  ")
	if err != nil {
		return err
	}
	return types.AtomicWriteFile(s.path, data, 00644)
}

// 根据id获取job
func (s *CronStore) Get(id string) *CronJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs {
		if j.ID == id {
			return j
		}
	}
	return nil
}

func (s *CronStore) SetEnable(id string, enabled bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs {
		if j.ID == id {
			j.Enabled = enabled
			if err := s.save(); err != nil {
				slog.Warn("cron: failed to save after set enabled", "error", err)
			}
			return true
		}
	}
	return false
}

// 设置id对应的Job mute值为mute
func (s *CronStore) SetMute(id string, mute bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs {
		if j.ID == id {
			j.Mute = mute
			if err := s.save(); err != nil {
				slog.Warn("cron: save after mute toggle", "error", err)
			}
			return true
		}
	}
	return false
}

// 从jobs中移除
func (s *CronStore) Remove(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, j := range s.jobs {
		if j.ID == id {
			s.jobs = append(s.jobs[:i], s.jobs[i+1:]...)
			if err := s.save(); err != nil {
				slog.Warn("cron: failed to save after remove", "error", err)
			}
			return true
		}
	}
	return false
}

// 根据sessionKey找到所有CronJob
func (s *CronStore) ListBySessionKey(sessionKey string) []*CronJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*CronJob
	for _, j := range s.jobs {
		if j.SessionKey == sessionKey {
			out = append(out, j)
		}
	}
	return out
}

// 通过向引擎注入同步消息来执行定时任务
type CronScheduler struct {
	store              *CronStore
	cron               *cron.Cron
	executor           CronExecutor
	mu                 sync.RWMutex
	entries            map[string]cron.EntryID // job ID -> cron entry
	defaultSilent      bool                    // 全局默认设置,用于抑制cron启动通知
	defaultSessionMode string                  // 全局默认session mode; "" = reuse, "new_per_run" = 刷新每个session
}

func NewCronScheduler(store *CronStore) *CronScheduler {
	return &CronScheduler{
		store:    store,
		cron:     cron.New(),
		executor: nil,
		entries:  make(map[string]cron.EntryID),
	}
}

func (cs *CronScheduler) Store() *CronStore {
	return cs.store
}

// job是否应该每次运行创建新的session, 同时考虑 job级别和全局默认
func (cs *CronScheduler) UsesNewSession(job *CronJob) bool {
	if job.SessionMode != "" {
		return job.UsesNewSessionPerRun()
	}
	return cs.defaultSessionMode == "new_per_run"
}

func (cs *CronScheduler) SetDefaultSilent(silent bool) {
	cs.defaultSilent = silent
}

// RegisterExecutor 替代 RegisterEngine
func (cs *CronScheduler) RegisterExecutor(executor CronExecutor) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.executor = executor
}

// ======================== CronScheduler:: job相关 =============================

func (s *CronStore) Add(job *CronJob) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs = append(s.jobs, job)
	return s.save()
}

func (cs *CronScheduler) AddJob(job *CronJob) error {
	if err := validateCronJob(job); err != nil {
		return err
	}
	job.SessionMode = NormalizeCronSessionMode(job.SessionMode)
	if _, err := cron.ParseStandard(job.CronExpr); err != nil {
		return fmt.Errorf("invalid cron expression %q: %w", job.CronExpr, err)
	}
	if err := cs.store.Add(job); err != nil {
		return err
	}
	if job.Enabled {
		return cs.scheduleJob(job)
	}
	return nil
}

// 从 CronScheduler entries 以及 CronStore中移除
func (cs *CronScheduler) RemoveJob(id string) bool {
	cs.mu.Lock()
	if entryID, ok := cs.entries[id]; ok {
		cs.cron.Remove(entryID)
		delete(cs.entries, id)
	}
	cs.mu.Unlock()
	return cs.store.Remove(id)
}

func (cs *CronScheduler) IsSilent(job *CronJob) bool {
	if job.Silent != nil {
		return *job.Silent
	}
	return cs.defaultSilent
}

// 设置job状态为Enable, 并启动
func (cs *CronScheduler) EnableJob(id string) error {
	if !cs.store.SetEnable(id, true) {
		return fmt.Errorf("job %q not found", id)
	}
	job := cs.store.Get(id)
	if job != nil {
		return cs.scheduleJob(job)
	}
	return nil
}

// 从entries中删除
func (cs *CronScheduler) DisableJob(id string) error {
	if !cs.store.SetEnable(id, false) {
		return fmt.Errorf("job %q not found", id)
	}
	cs.mu.Lock()
	if entryID, ok := cs.entries[id]; ok {
		cs.cron.Remove(entryID)
		delete(cs.entries, id)
	}
	cs.mu.Unlock()
	return nil
}

// 启用定时任务
func (cs *CronScheduler) scheduleJob(job *CronJob) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	// 若存在定时任务, 移除
	if old, ok := cs.entries[job.ID]; ok {
		cs.cron.Remove(old)
	}

	jobID := job.ID
	entryID, err := cs.cron.AddFunc(job.CronExpr, func() {
		cs.executeJob(jobID)
	})
	if err != nil {
		return err
	}
	cs.entries[jobID] = entryID
	return nil
}

func (cs *CronScheduler) executeJob(jobID string) {
	job := cs.store.Get(jobID)
	if job == nil || !job.Enabled {
		return
	}

	cs.mu.Lock()
	executor := cs.executor
	cs.mu.RUnlock()

	slog.Info("cron: executing job", "id", jobID, "project", job.Project, "prompt", types.TruncateStr(job.Prompt, 60))

	done := make(chan error, 1)
	go func() {
		done <- executor.ExecuteCronJob(job)
	}()

	var err error
	timeout := job.ExecutionTimeout()
	if timeout > 0 {
		select {
		case err = <-done:
		case <-time.After(timeout):
			err = fmt.Errorf("job timed out after %v", timeout)
		}
	} else {
		err = <-done
	}

	cs.store.MarkRun(jobID, err)

	if err != nil {
		slog.Error("cron: job failed", "id", jobID, "error", err)
	} else {
		slog.Info("cron: job completed", "id", jobID)
	}
}

// 返回job下一次定时运行的时间, 如果没有scheduled 返回0
func (cs *CronScheduler) NextRun(jobID string) time.Time {
	cs.mu.RLock()
	entryID, ok := cs.entries[jobID]
	cs.mu.RUnlock()
	if !ok {
		return time.Time{}
	}
	for _, e := range cs.cron.Entries() {
		if e.ID == entryID {
			return e.Next
		}
	}
	return time.Time{}
}

// ============================= 辅助函数 ===================================

// Pure interval: */N * * * * → "每N 分钟"
func parseStep(field string) (int, bool) {
	if !strings.HasPrefix(field, "*/") {
		return 0, false
	}
	var n int
	if _, err := fmt.Sscanf(field[2:], "%d", &n); err == nil && n > 0 {
		return n, true
	}
	return 0, false
}

// 补0至2位
func padZero(s string) string {
	if len(s) == 1 {
		return "0" + s
	}
	return s
}

// 将 min, hour, dom, month, dow 五个字段的表示转化为人类可读形式
func CronExprToHuman(expr string, lang i18n.Language) string {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return expr
	}
	minute, hour, dom, month, dow := fields[0], fields[1], fields[2], fields[3], fields[4]
	weekdays, months := i18n.CronLangNames(lang)
	cjk := i18n.IsZhLikeLang(lang)
	allWild := dom == "*" && month == "*" && dow == "*"

	// Pure interval: */N * * * * → "Every N minutes"
	if minStep, ok := parseStep(minute); ok && hour == "*" && allWild {
		switch lang {
		case i18n.LangChinese:
			return fmt.Sprintf("每%d分钟", minStep)
		case i18n.LangTraditionalChinese:
			return fmt.Sprintf("每%d分鐘", minStep)
		case i18n.LangJapanese:
			return fmt.Sprintf("%d分ごと", minStep)
		default:
			return fmt.Sprintf("Every %d min", minStep)
		}
	}

	// Hour interval: M */N * * * → "Every N hours (:MM)"
	if hourStep, ok := parseStep(hour); ok && allWild {
		m := padZero(minute)
		if minute == "*" {
			m = "00"
		}
		switch lang {
		case i18n.LangChinese:
			return fmt.Sprintf("每%d小时 (:%s)", hourStep, m)
		case i18n.LangTraditionalChinese:
			return fmt.Sprintf("每%d小時 (:%s)", hourStep, m)
		case i18n.LangJapanese:
			return fmt.Sprintf("%d時間ごと (:%s)", hourStep, m)
		default:
			return fmt.Sprintf("Every %d h (:%s)", hourStep, m)
		}
	}

	var parts []string

	// Weekday
	if dow != "*" {
		if n, err := strconv.Atoi(dow); err == nil && n >= 0 && n <= 6 {
			if cjk {
				parts = append(parts, weekdays[n])
			} else {
				parts = append(parts, "Every "+weekdays[n])
			}
		} else {
			parts = append(parts, "weekday("+dow+")")
		}
	}

	// Month
	if month != "*" {
		if n, err := strconv.Atoi(month); err == nil && n >= 1 && n <= 12 {
			parts = append(parts, months[n])
		}
	}

	// Day of month
	if dom != "*" {
		if cjk {
			parts = append(parts, dom+"日")
		} else {
			parts = append(parts, "day "+dom)
		}
	}

	// Time
	if hour != "*" && minute != "*" {
		if minStep, ok := parseStep(minute); ok {
			switch lang {
			case i18n.LangChinese, i18n.LangTraditionalChinese:
				parts = append(parts, fmt.Sprintf("%s时 每%d分钟", padZero(hour), minStep))
			case i18n.LangJapanese:
				parts = append(parts, fmt.Sprintf("%s時 %d分ごと", padZero(hour), minStep))
			default:
				parts = append(parts, fmt.Sprintf("hour %s every %d min", padZero(hour), minStep))
			}
		} else {
			parts = append(parts, fmt.Sprintf("%s:%s", padZero(hour), padZero(minute)))
		}
	} else if hour != "*" {
		if cjk {
			parts = append(parts, hour+"時")
		} else {
			parts = append(parts, "hour "+hour)
		}
	} else if minute != "*" {
		if minStep, ok := parseStep(minute); ok {
			switch lang {
			case i18n.LangChinese:
				parts = append(parts, fmt.Sprintf("每%d分钟", minStep))
			case i18n.LangTraditionalChinese:
				parts = append(parts, fmt.Sprintf("每%d分鐘", minStep))
			case i18n.LangJapanese:
				parts = append(parts, fmt.Sprintf("%d分ごと", minStep))
			default:
				parts = append(parts, fmt.Sprintf("every %d min", minStep))
			}
		} else {
			switch lang {
			case i18n.LangChinese, i18n.LangTraditionalChinese:
				parts = append(parts, "每小时第"+minute+"分")
			case i18n.LangJapanese:
				parts = append(parts, "毎時"+minute+"分")
			default:
				parts = append(parts, "minute "+minute+" of every hour")
			}
		}
	}

	// Frequency hint
	if allWild {
		switch lang {
		case i18n.LangChinese, i18n.LangTraditionalChinese:
			return "每天 " + strings.Join(parts, " ")
		case i18n.LangJapanese:
			return "毎日 " + strings.Join(parts, " ")
		default:
			return "Daily at " + strings.Join(parts, " ")
		}
	}
	if dow != "*" && month == "*" && dom == "*" {
		switch lang {
		case i18n.LangChinese, i18n.LangTraditionalChinese:
			return "每" + strings.Join(parts, " ")
		case i18n.LangJapanese:
			return "毎" + strings.Join(parts, " ")
		default:
			return strings.Join(parts, " at ")
		}
	}
	if dom != "*" && month == "*" && dow == "*" {
		switch lang {
		case i18n.LangChinese, i18n.LangTraditionalChinese:
			return "每月" + strings.Join(parts, " ")
		case i18n.LangJapanese:
			return "毎月" + strings.Join(parts, " ")
		default:
			return "Monthly, " + strings.Join(parts, ", ")
		}
	}

	if cjk {
		return strings.Join(parts, " ")
	}
	return strings.Join(parts, ", ")
}

// 映射CLI/API 别名到 规范值("", "new_per_run").
// 如果未识别 返回原始string (调用者应合法)
func NormalizeCronSessionMode(s string) string {
	s = strings.TrimSpace(s)
	low := strings.ToLower(s)
	switch low {
	case "", "reuse":
		return ""
	case "new_per_run", "new-per-run":
		return "new_per_run"
	default:
		return s
	}
}

// 生成唯一定时任务
func GenerateCronID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Errorf("generate cron id: %w", err))
	}
	return hex.EncodeToString(b)
}

func validateCronJob(j *CronJob) error {
	// 判断SessionMode 是否为合法 new_per_run
	mode := NormalizeCronSessionMode(j.SessionMode)
	if mode != "" && mode != "new_per_run" {
		return fmt.Errorf("invalid session_mode %q (want reuse, new_per_run, or new-per-run)", j.SessionMode)
	}
	// 判断mode 是否为合法 default | bypassPermissions | acceptEdits | plan | auto | dontAsk
	if j.Mode != "" {
		switch j.Mode {
		case "default", "bypassPermissions", "acceptEdits", "plan", "auto", "dontAsk":
		default:
			return fmt.Errorf("invalid mode %q (want default, bypassPermissions, acceptEdits, plan, auto, or dontAsk)", j.Mode)
		}
	}
	// 判断超时时间>0
	if j.TimeoutMins != nil && *j.TimeoutMins < 0 {
		return fmt.Errorf("timeout_mins must be >= 0")
	}
	return nil
}

// t 和 now 是否跨年决定格式是否带 年份
func CronTimeFormat(t, now time.Time) string {
	if t.Year() != now.Year() {
		return "2006-01-02 15:04"
	}
	return "01-02 15:04"
}
