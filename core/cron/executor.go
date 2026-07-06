package cron

// CronExecutor 执行 cron job 的接口
type CronExecutor interface {
	ExecuteCronJob(job *CronJob) error
}
