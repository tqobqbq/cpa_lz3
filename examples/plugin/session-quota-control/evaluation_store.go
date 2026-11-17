package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/examples/plugin/session-quota-control/evaluation"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	log "github.com/sirupsen/logrus"
)

const evaluationHistoryLimit = 100
const evaluationConcurrency = 4

type evaluationJob struct {
	ID           string    `json:"id"`
	BatchID      string    `json:"batch_id"`
	CredentialID string    `json:"credential_id"`
	Provider     string    `json:"provider"`
	CreatedAt    time.Time `json:"created_at"`
	ScheduledAt  time.Time `json:"scheduled_at"`
	StartedAt    time.Time `json:"started_at,omitempty"`
	FinishedAt   time.Time `json:"finished_at,omitempty"`
	evaluation.Options
	Result evaluation.Result `json:"result"`
}

type evaluationManager struct {
	lifecycle sync.Mutex
	mu        sync.Mutex
	db        *sql.DB
	path      string
	lastErr   string
	ctx       context.Context
	cancel    context.CancelFunc
	done      chan struct{}
	wake      chan struct{}
	active    map[string]context.CancelFunc
	workers   sync.WaitGroup
	now       func() time.Time
	auths     func() ([]pluginapi.HostAuthFileEntry, error)
	run       func(context.Context, evaluation.Credential, evaluation.Options, func(evaluation.Result)) evaluation.Result
}

func newEvaluationManager(auths func() ([]pluginapi.HostAuthFileEntry, error)) *evaluationManager {
	runner := evaluation.NewRunner(func(method string, request any) (json.RawMessage, error) {
		var response json.RawMessage
		errCall := callHost(method, request, &response)
		return response, errCall
	})
	return &evaluationManager{now: time.Now, auths: auths, run: runner.Run}
}

func (s *evaluationManager) configure(path string) error {
	resolved, errPath := reportPath(path)
	if errPath != nil {
		return errPath
	}
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	s.mu.Lock()
	current := s.db != nil && s.path == resolved && s.ctx.Err() == nil
	s.mu.Unlock()
	if current {
		return nil
	}
	s.stopLocked()
	if errMkdir := os.MkdirAll(filepath.Dir(resolved), 0o700); errMkdir != nil {
		return fmt.Errorf("create evaluation directory: %w", errMkdir)
	}
	file, errCreate := os.OpenFile(resolved, os.O_CREATE|os.O_RDWR, 0o600)
	if errCreate != nil {
		return fmt.Errorf("create evaluation database: %w", errCreate)
	}
	if errClose := file.Close(); errClose != nil {
		return fmt.Errorf("close evaluation database file: %w", errClose)
	}
	dsn := &url.URL{Scheme: "file", Path: filepath.ToSlash(resolved)}
	dsn.RawQuery = url.Values{"_pragma": {"busy_timeout(5000)", "synchronous(NORMAL)"}}.Encode()
	db, errOpen := sql.Open("sqlite", dsn.String())
	if errOpen != nil {
		return fmt.Errorf("open evaluation database: %w", errOpen)
	}
	db.SetMaxOpenConns(1)
	for _, statement := range []string{
		"PRAGMA journal_mode=WAL",
		"CREATE TABLE IF NOT EXISTS evaluation_jobs (id TEXT PRIMARY KEY, credential_id TEXT NOT NULL, created_at INTEGER NOT NULL, scheduled_at INTEGER NOT NULL, status TEXT NOT NULL, payload TEXT NOT NULL)",
		"CREATE INDEX IF NOT EXISTS evaluation_jobs_credential ON evaluation_jobs(credential_id, created_at DESC)",
		"CREATE INDEX IF NOT EXISTS evaluation_jobs_schedule ON evaluation_jobs(status, scheduled_at)",
		`UPDATE evaluation_jobs SET status='interrupted', payload=json_set(payload, '$.result.status', 'interrupted', '$.result.error', '服务重启前的检测未完成') WHERE status IN ('running','cancelling')`,
	} {
		if _, errExec := db.Exec(statement); errExec != nil {
			if errClose := db.Close(); errClose != nil {
				log.WithError(errClose).Warn("session-quota-control: close evaluation database")
			}
			return fmt.Errorf("initialize evaluation database: %w", errExec)
		}
	}
	s.mu.Lock()
	s.db, s.path, s.lastErr = db, resolved, ""
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.done, s.wake = make(chan struct{}), make(chan struct{}, 1)
	s.active = make(map[string]context.CancelFunc)
	s.mu.Unlock()
	go s.loop()
	return nil
}

func (s *evaluationManager) stop() {
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	s.stopLocked()
}

func (s *evaluationManager) stopLocked() {
	s.mu.Lock()
	cancel, done := s.cancel, s.done
	s.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
		s.workers.Wait()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db != nil {
		if errClose := s.db.Close(); errClose != nil {
			log.WithError(errClose).Warn("session-quota-control: close evaluation database")
		}
	}
	s.db, s.cancel, s.done = nil, nil, nil
}

func (s *evaluationManager) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// The dispatcher uses persisted absolute times. Browser visibility and a slow
// earlier test do not change when the next batch becomes eligible.
func (s *evaluationManager) loop() {
	defer close(s.done)
	for {
		delay := s.dispatch()
		timer := time.NewTimer(delay)
		select {
		case <-s.ctx.Done():
			timer.Stop()
			return
		case <-s.wake:
			timer.Stop()
		case <-timer.C:
		}
	}
}

func (s *evaluationManager) dispatch() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil {
		return time.Hour
	}
	for len(s.active) < evaluationConcurrency {
		var payload []byte
		errRead := s.db.QueryRow("SELECT payload FROM evaluation_jobs WHERE status='scheduled' ORDER BY scheduled_at, created_at, id LIMIT 1").Scan(&payload)
		if errors.Is(errRead, sql.ErrNoRows) {
			return time.Hour
		}
		if errRead != nil {
			s.lastErr = errRead.Error()
			return time.Minute
		}
		var job evaluationJob
		if errDecode := json.Unmarshal(payload, &job); errDecode != nil {
			s.lastErr = errDecode.Error()
			return time.Minute
		}
		if delay := job.ScheduledAt.Sub(s.now()); delay > 0 {
			return delay
		}
		job.StartedAt, job.Result.Status = s.now(), "running"
		if errSave := s.saveLocked(job); errSave != nil {
			return time.Minute
		}
		ctx, cancel := context.WithCancel(s.ctx)
		s.active[job.ID] = cancel
		s.workers.Add(1)
		go s.execute(ctx, job)
	}
	return time.Hour
}

func (s *evaluationManager) execute(ctx context.Context, job evaluationJob) {
	defer s.workers.Done()
	result := evaluation.Result{Status: "failed"}
	defer func() {
		if recovered := recover(); recovered != nil {
			result.Status, result.Error = "failed", fmt.Sprintf("检测失败：%v", recovered)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.ctx.Err() != nil {
			result.Status, result.Error = "interrupted", "服务停止，检测未完成"
		} else if ctx.Err() != nil {
			result.Status, result.Error = "cancelled", "检测已取消"
		}
		job.Result, job.FinishedAt = result, s.now()
		_ = s.saveLocked(job)
		s.active[job.ID]()
		delete(s.active, job.ID)
		s.pruneLocked(job.CredentialID)
		s.signal()
	}()
	auths, errAuths := s.auths()
	if errAuths != nil {
		result.Error = "无法读取 Credential"
		return
	}
	index := slices.IndexFunc(auths, func(auth pluginapi.HostAuthFileEntry) bool {
		return auth.ID == job.CredentialID && auth.Provider == job.Provider
	})
	if index < 0 || auths[index].Disabled {
		result.Status, result.Error = "skipped", "Credential 已禁用或移除"
		return
	}
	result = s.run(ctx, evaluation.Credential{ID: job.CredentialID, Provider: job.Provider}, job.Options, func(progress evaluation.Result) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if ctx.Err() == nil {
			job.Result = progress
			_ = s.saveLocked(job)
		}
	})
}

func (s *evaluationManager) saveLocked(job evaluationJob) error {
	payload, errEncode := json.Marshal(job)
	if errEncode != nil {
		s.lastErr = errEncode.Error()
		return errEncode
	}
	_, errSave := s.db.Exec(`INSERT INTO evaluation_jobs(id, credential_id, created_at, scheduled_at, status, payload)
		VALUES(?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET status=excluded.status, payload=excluded.payload`,
		job.ID, job.CredentialID, job.CreatedAt.UnixNano(), job.ScheduledAt.UnixNano(), job.Result.Status, payload)
	if errSave != nil {
		s.lastErr = errSave.Error()
	}
	return errSave
}

func (s *evaluationManager) pruneLocked(credentialID string) {
	_, errPrune := s.db.Exec(`DELETE FROM evaluation_jobs WHERE id IN (
		SELECT id FROM evaluation_jobs WHERE credential_id=? AND status NOT IN ('scheduled','running','cancelling')
		ORDER BY created_at DESC, scheduled_at DESC, id DESC LIMIT -1 OFFSET ?)`, credentialID, evaluationHistoryLimit)
	if errPrune != nil {
		s.lastErr = errPrune.Error()
	}
}

type evaluationPlan struct {
	CredentialID string   `json:"credential_id"`
	Model        string   `json:"model"`
	Effort       string   `json:"effort"`
	Tests        []string `json:"tests"`
	Delays       []int    `json:"delays"`
	Variant      string   `json:"variant"`
}

func defaultEvaluationPlan() evaluationPlan {
	return evaluationPlan{Model: evaluation.DefaultModel, Effort: evaluation.DefaultEffort,
		Tests: []string{"fingerprint", "pelican"}, Delays: []int{0, 300}, Variant: "original"}
}

func (p evaluationPlan) options() ([]evaluation.Options, error) {
	if p.CredentialID == "" || len(p.Tests) == 0 || len(p.Tests) > 3 || len(p.Delays) == 0 || len(p.Delays) > 8 {
		return nil, fmt.Errorf("请选择 Credential、检测项目和检测时间")
	}
	options, seenTests, seenDelays := []evaluation.Options{}, map[string]bool{}, map[int]bool{}
	for _, test := range p.Tests {
		if seenTests[test] {
			return nil, fmt.Errorf("检测项目不能重复")
		}
		seenTests[test] = true
		option := evaluation.Options{Model: p.Model, Effort: p.Effort, Test: test}
		if test == "pelican" {
			option.Variant = p.Variant
		}
		if errValidate := option.Validate(); errValidate != nil {
			return nil, errValidate
		}
		options = append(options, option)
	}
	for _, delay := range p.Delays {
		if delay < 0 || delay > 86400 || seenDelays[delay] {
			return nil, fmt.Errorf("检测时间须为不重复的 0～86400 秒")
		}
		seenDelays[delay] = true
	}
	return options, nil
}

func (s *evaluationManager) add(plan evaluationPlan, provider string) ([]evaluationJob, error) {
	options, errValidate := plan.options()
	if errValidate != nil {
		return nil, errValidate
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil || s.ctx.Err() != nil {
		return nil, fmt.Errorf("检测服务未启动")
	}
	var pending int
	if errCount := s.db.QueryRow("SELECT count(*) FROM evaluation_jobs WHERE status IN ('scheduled','running','cancelling')").Scan(&pending); errCount != nil {
		return nil, errCount
	}
	if pending+len(options)*len(plan.Delays) > 256 {
		return nil, fmt.Errorf("待执行检测过多，请等待或取消已有检测")
	}
	tx, errBegin := s.db.Begin()
	if errBegin != nil {
		return nil, errBegin
	}
	defer func() {
		if errRollback := tx.Rollback(); errRollback != nil && !errors.Is(errRollback, sql.ErrTxDone) {
			log.WithError(errRollback).Warn("session-quota-control: roll back evaluation schedule")
		}
	}()
	batchID, now := uuid.NewString(), s.now()
	jobs := make([]evaluationJob, 0, len(options)*len(plan.Delays))
	for _, delay := range plan.Delays {
		for _, option := range options {
			job := evaluationJob{ID: uuid.NewString(), BatchID: batchID, CredentialID: plan.CredentialID,
				Provider: provider, CreatedAt: now, ScheduledAt: now.Add(time.Duration(delay) * time.Second),
				Options: option, Result: evaluation.Result{Status: "scheduled", Total: 1}}
			if option.Test == "fingerprint" {
				job.Result.Total = 3
			}
			payload, errEncode := json.Marshal(job)
			if errEncode != nil {
				return nil, errEncode
			}
			if _, errInsert := tx.Exec("INSERT INTO evaluation_jobs VALUES(?,?,?,?,?,?)",
				job.ID, job.CredentialID, job.CreatedAt.UnixNano(), job.ScheduledAt.UnixNano(), job.Result.Status, payload); errInsert != nil {
				return nil, errInsert
			}
			jobs = append(jobs, job)
		}
	}
	if errCommit := tx.Commit(); errCommit != nil {
		return nil, errCommit
	}
	s.signal()
	return jobs, nil
}

func (s *evaluationManager) list(credentialID string) ([]evaluationJob, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil, "", fmt.Errorf("检测服务未启动")
	}
	rows, errQuery := s.db.Query(`SELECT payload FROM evaluation_jobs WHERE credential_id=?
		ORDER BY created_at DESC, scheduled_at, id LIMIT ?`, credentialID, evaluationHistoryLimit+256)
	if errQuery != nil {
		return nil, "", errQuery
	}
	defer func() {
		if errClose := rows.Close(); errClose != nil {
			log.WithError(errClose).Warn("session-quota-control: close evaluation history rows")
		}
	}()
	jobs := []evaluationJob{}
	for rows.Next() {
		var payload []byte
		if errScan := rows.Scan(&payload); errScan != nil {
			return nil, "", errScan
		}
		var job evaluationJob
		if errDecode := json.Unmarshal(payload, &job); errDecode != nil {
			return nil, "", errDecode
		}
		job.Result = job.Result.Summary()
		jobs = append(jobs, job)
	}
	return jobs, s.lastErr, rows.Err()
}

func (s *evaluationManager) recordLocked(credentialID, id string) (evaluationJob, error) {
	var job evaluationJob
	if s.db == nil {
		return job, fmt.Errorf("检测服务未启动")
	}
	var payload []byte
	if errRead := s.db.QueryRow("SELECT payload FROM evaluation_jobs WHERE credential_id=? AND id=?", credentialID, id).Scan(&payload); errRead != nil {
		return job, errRead
	}
	errDecode := json.Unmarshal(payload, &job)
	return job, errDecode
}

func (s *evaluationManager) record(credentialID, id string) (evaluationJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recordLocked(credentialID, id)
}

func (s *evaluationManager) cancelJob(credentialID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, errRead := s.recordLocked(credentialID, id)
	if errRead != nil {
		return errRead
	}
	switch job.Result.Status {
	case "scheduled":
		job.Result.Status, job.FinishedAt = "cancelled", s.now()
	case "running", "cancelling":
		job.Result.Status = "cancelling"
		if cancel := s.active[id]; cancel != nil {
			cancel()
		}
	default:
		return fmt.Errorf("检测已结束")
	}
	if errSave := s.saveLocked(job); errSave != nil {
		return errSave
	}
	if job.Result.Status == "cancelled" {
		s.pruneLocked(credentialID)
	}
	s.signal()
	return nil
}
