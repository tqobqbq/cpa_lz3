package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/examples/plugin/session-quota-control/evaluation"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	log "github.com/sirupsen/logrus"
)

type hubChannelResult struct {
	Channel hubChannel        `json:"channel"`
	Result  evaluation.Result `json:"result"`
}

func hubPassed(r evaluation.Result) bool {
	return r.Status == "completed" && r.ModelMatch != nil && *r.ModelMatch && r.Attribution != nil && r.Attribution.Used == 3 && r.Attribution.Decision != "not_model"
}

type hubJob struct {
	ID         string             `json:"id"`
	Settings   hubSettings        `json:"settings"`
	CreatedAt  time.Time          `json:"created_at"`
	FinishedAt time.Time          `json:"finished_at"`
	AppliedAt  time.Time          `json:"applied_at"`
	Status     string             `json:"status"`
	Error      string             `json:"error,omitempty"`
	Results    []hubChannelResult `json:"results"`
}

// A write-ahead snapshot survives cancellation, lost mutation responses and
// process termination. Settings cannot change targets while recovery is pending.
type hubRecovery struct {
	Kind     string      `json:"kind"`
	JobID    string      `json:"job_id"`
	Original hubProfiles `json:"original"`
	Previous hubProfiles `json:"previous"`
	Last     hubProfiles `json:"last"`
}

type hubDisk struct {
	Settings hubSettings  `json:"settings"`
	Channels []hubChannel `json:"channels"`
	Source   string       `json:"source"`
	FoundAt  time.Time    `json:"found_at"`
	Jobs     []*hubJob    `json:"jobs"`
	Recovery *hubRecovery `json:"recovery,omitempty"`
}

type hubManager struct {
	lifecycle    sync.Mutex
	mu           sync.Mutex
	workers      sync.WaitGroup
	path         string
	disk         hubDisk
	ctx          context.Context
	stopWork     context.CancelFunc
	cancel       context.CancelFunc
	busy         bool
	action       string
	message      string
	storageError string
	// Tests replace the transport-bound runner without changing job orchestration.
	run    func(context.Context, *hubClient, string, evaluation.Options, func(evaluation.Result)) evaluation.Result
	settle func(context.Context) error
}

func newHubManager() *hubManager {
	return &hubManager{disk: hubDisk{Settings: defaultHubSettings()}, settle: func(ctx context.Context) error {
		// Match hub/'s propagation pause without imposing a deadline on requests.
		timer := time.NewTimer(500 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}, run: func(ctx context.Context, client *hubClient, secret string, options evaluation.Options, progress func(evaluation.Result)) evaluation.Result {
		return evaluation.NewProbeRunner(client.probe(secret)).Run(ctx, evaluation.Credential{}, options, progress)
	}}
}

func (s *hubManager) configure(path string) error {
	resolved, errPath := reportPath(path)
	if errPath != nil {
		return errPath
	}
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	s.mu.Lock()
	unchanged := s.path == resolved && s.ctx != nil && s.ctx.Err() == nil
	s.mu.Unlock()
	if unchanged {
		return nil
	}
	s.stopLocked()
	disk := hubDisk{Settings: defaultHubSettings()}
	raw, errRead := os.ReadFile(resolved)
	if errRead == nil {
		if errDecode := json.Unmarshal(raw, &disk); errDecode != nil {
			return fmt.Errorf("无法读取 Hub 保存的数据")
		}
	} else if !errors.Is(errRead, os.ErrNotExist) {
		return fmt.Errorf("无法打开 Hub 数据文件")
	}
	for _, job := range disk.Jobs {
		if job.Status == "running" {
			job.Status, job.Error = "interrupted", "服务重启，检测已中断"
			for i := range job.Results {
				if slices.Contains([]string{"running", "queued"}, job.Results[i].Result.Status) {
					job.Results[i].Result.Status = "interrupted"
				}
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.path, s.disk = resolved, disk
	s.ctx, s.stopWork = context.WithCancel(context.Background())
	return s.saveLocked()
}

func (s *hubManager) stop() {
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	s.stopLocked()
}

func (s *hubManager) stopLocked() {
	s.mu.Lock()
	if s.stopWork != nil {
		s.stopWork()
	}
	s.mu.Unlock()
	s.workers.Wait()
}

func (s *hubManager) saveLocked() error {
	errSave := s.writeLocked()
	if errSave != nil {
		s.storageError = "Hub 数据保存失败，请检查目录权限和磁盘空间"
		return errors.New(s.storageError)
	}
	s.storageError = ""
	return nil
}

func (s *hubManager) writeLocked() error {
	if s.path == "" {
		return fmt.Errorf("Hub 尚未初始化")
	}
	if errMkdir := os.MkdirAll(filepath.Dir(s.path), 0o700); errMkdir != nil {
		return errMkdir
	}
	raw, errMarshal := json.Marshal(s.disk)
	if errMarshal != nil {
		return errMarshal
	}
	file, errCreate := os.CreateTemp(filepath.Dir(s.path), ".hub-*.tmp")
	if errCreate != nil {
		return errCreate
	}
	defer func() {
		if errRemove := os.Remove(file.Name()); errRemove != nil && !errors.Is(errRemove, os.ErrNotExist) {
			log.Debug("session-quota-control: could not remove Hub temporary file")
		}
	}()
	_, errWrite := file.Write(raw)
	if errWrite == nil {
		errWrite = file.Sync()
	}
	errClose := file.Close()
	if errWrite != nil {
		return errWrite
	}
	if errClose != nil {
		return errClose
	}
	return os.Rename(file.Name(), s.path)
}

func (s *hubManager) settings(body []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy {
		return fmt.Errorf("请等待当前 Hub 操作结束后保存配置")
	}
	next := s.disk.Settings
	if errDecode := json.Unmarshal(body, &next); errDecode != nil {
		return fmt.Errorf("Hub 配置格式错误")
	}
	// Empty password inputs retain saved secrets; explicit clearing is separate.
	var flags struct {
		ClearAuthorization bool `json:"clear_authorization"`
		ClearCookie        bool `json:"clear_cookie"`
	}
	_ = json.Unmarshal(body, &flags)
	next.Authorization, next.Cookie = strings.TrimSpace(next.Authorization), strings.TrimSpace(next.Cookie)
	if next.Authorization == "" {
		next.Authorization = s.disk.Settings.Authorization
	}
	if next.Cookie == "" {
		next.Cookie = s.disk.Settings.Cookie
	}
	if flags.ClearAuthorization {
		next.Authorization = ""
	}
	if flags.ClearCookie {
		next.Cookie = ""
	}
	if next.Authorization != "" {
		if len(next.Authorization) > 7 && strings.EqualFold(next.Authorization[:7], "Bearer ") {
			next.Authorization = strings.TrimSpace(next.Authorization[7:])
		}
		next.Authorization = "Bearer " + next.Authorization
	}
	next.Model, next.Filter = strings.TrimSpace(next.Model), strings.TrimSpace(next.Filter)
	next.GraphQLURL, next.APIBaseURL, next.ProxyURL = strings.TrimSpace(next.GraphQLURL), strings.TrimRight(strings.TrimSpace(next.APIBaseURL), "/"), strings.TrimSpace(next.ProxyURL)
	if errValidate := next.validate(); errValidate != nil {
		return errValidate
	}
	if s.disk.Recovery != nil && !sameHubTarget(next, s.disk.Settings) {
		return fmt.Errorf("请先恢复 Key 绑定；恢复前只可更新 Authorization、Cookie 或代理")
	}
	previous := s.disk.Settings
	s.disk.Settings = next
	if errSave := s.saveLocked(); errSave != nil {
		s.disk.Settings = previous
		return errSave
	}
	return nil
}

func sameHubTarget(a, b hubSettings) bool {
	return a.KeyID == b.KeyID && a.ProjectID == b.ProjectID && a.GraphQLURL == b.GraphQLURL
}

func (s *hubManager) state() pluginapi.ManagementResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	jobs := make([]hubJob, 0, len(s.disk.Jobs))
	for _, original := range s.disk.Jobs {
		job := *original
		job.Results = make([]hubChannelResult, len(original.Results))
		for i, result := range original.Results {
			job.Results[i] = hubChannelResult{result.Channel, result.Result.Summary()}
		}
		jobs = append(jobs, job)
	}
	return jsonResponse(200, map[string]any{"settings": s.disk.Settings.public(), "authorization_saved": s.disk.Settings.Authorization != "", "cookie_saved": s.disk.Settings.Cookie != "",
		"channels": s.disk.Channels, "source": s.disk.Source, "found_at": s.disk.FoundAt, "jobs": jobs,
		"busy": s.busy, "action": s.action, "message": s.message, "recovery_required": s.disk.Recovery != nil,
		"recovery":      s.disk.Recovery,
		"storage_error": s.storageError, "models": evaluation.ModelNames()})
}

func (s *hubManager) record(id string, channelID int64) pluginapi.ManagementResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, job := range s.disk.Jobs {
		if job.ID != id {
			continue
		}
		for _, result := range job.Results {
			if result.Channel.ID == channelID {
				return jsonResponse(200, map[string]any{"id": job.ID, "test": "fingerprint", "model": job.Settings.Model, "effort": job.Settings.Effort,
					"scheduled_at": job.CreatedAt, "channel": result.Channel, "key_id": job.Settings.KeyID, "result": result.Result})
			}
		}
	}
	return jsonResponse(404, map[string]string{"error": "找不到该渠道的检测记录"})
}

func (s *hubManager) launch(action string, ids []int64, jobID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx == nil || s.ctx.Err() != nil {
		return fmt.Errorf("Hub 尚未初始化")
	}
	if s.busy {
		return fmt.Errorf("已有 Hub 操作正在执行")
	}
	if s.disk.Recovery != nil && action != "recover" {
		return fmt.Errorf("上次渠道绑定尚未恢复，请先恢复绑定")
	}
	settings := s.disk.Settings
	if errValidate := settings.validate(); errValidate != nil {
		return errValidate
	}
	if settings.Authorization == "" {
		return fmt.Errorf("请先保存 Authorization")
	}
	if action != "discover" && settings.KeyID <= 0 {
		return fmt.Errorf("请先保存 KEY_ID")
	}
	client, errClient := newHubClient(settings)
	if errClient != nil {
		return errClient
	}
	var job *hubJob
	switch action {
	case "detect":
		options := evaluation.Options{Model: settings.Model, Effort: settings.Effort, Test: "fingerprint"}
		if errValidate := options.Validate(); errValidate != nil {
			return errValidate
		}
		if len(ids) == 0 || len(ids) > 100 {
			return fmt.Errorf("每次请选择 1～100 个渠道")
		}
		ids = slices.Clone(ids)
		slices.Sort(ids)
		ids = slices.Compact(ids)
		if ids[0] <= 0 || ids[len(ids)-1] > 9007199254740991 {
			return fmt.Errorf("渠道 ID 无效")
		}
		job = &hubJob{ID: uuid.NewString(), Settings: settings.public(), CreatedAt: time.Now(), Status: "running"}
		for _, id := range ids {
			channel := hubChannel{ID: id, Name: fmt.Sprintf("Channel #%d", id)}
			for _, found := range s.disk.Channels {
				if found.ID == id {
					channel = found
					break
				}
			}
			job.Results = append(job.Results, hubChannelResult{channel, evaluation.Result{Status: "queued", Total: 3}})
		}
		s.disk.Jobs = append([]*hubJob{job}, s.disk.Jobs...)
		s.disk.Jobs = s.disk.Jobs[:min(len(s.disk.Jobs), 10)]
		s.disk.Settings.ChannelIDs = ids
		if errSave := s.saveLocked(); errSave != nil {
			job.Status = "failed"
			return errSave
		}
	case "apply":
		for _, found := range s.disk.Jobs {
			if found.ID == jobID {
				job = found
				break
			}
		}
		if job == nil || job.Status != "completed" {
			return fmt.Errorf("请选择已完成的检测记录")
		}
		if !sameHubTarget(job.Settings, settings) || job.Settings.Model != settings.Model || job.Settings.APIBaseURL != settings.APIBaseURL || job.Settings.Protocol != settings.Protocol || job.Settings.Effort != settings.Effort {
			return fmt.Errorf("当前 KEY_ID、模型或接口与该检测记录不同，请恢复对应配置后应用")
		}
		if !slices.ContainsFunc(job.Results, func(r hubChannelResult) bool { return hubPassed(r.Result) }) {
			return fmt.Errorf("没有完整检测且模型一致的通过渠道")
		}
	case "recover":
		if s.disk.Recovery == nil {
			return fmt.Errorf("没有待恢复的绑定")
		}
	case "discover", "key-channels":
	default:
		return fmt.Errorf("未知 Hub 操作")
	}
	ctx, cancel := context.WithCancel(s.ctx)
	s.cancel, s.busy, s.action, s.message = cancel, true, action, ""
	s.workers.Add(1)
	go func() {
		defer s.workers.Done()
		defer cancel()
		defer client.http.CloseIdleConnections()
		var errRun error
		switch action {
		case "discover":
			var channels []hubChannel
			channels, errRun = client.discover(ctx)
			if errRun == nil {
				s.mu.Lock()
				s.disk.Channels, s.disk.Source, s.disk.FoundAt = channels, "free", time.Now()
				errRun = s.saveLocked()
				s.mu.Unlock()
			}
		case "key-channels":
			var key *hubKey
			key, errRun = client.key(ctx)
			if errRun == nil {
				channels := []hubChannel{}
				for _, id := range key.Profiles.channelIDs() {
					channels = append(channels, hubChannel{ID: id, Name: fmt.Sprintf("Channel #%d", id)})
				}
				s.mu.Lock()
				s.disk.Channels, s.disk.Source, s.disk.FoundAt = channels, "key", time.Now()
				errRun = s.saveLocked()
				s.mu.Unlock()
			}
		case "detect":
			errRun = s.detect(ctx, client, job)
		case "apply":
			errRun = s.apply(ctx, client, job)
		case "recover":
			errRun = s.recover(ctx, client)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		s.busy, s.cancel = false, nil
		if errRun != nil {
			s.message = client.redact(errRun.Error())
		} else {
			s.message = map[string]string{"discover": "免费渠道列表已更新，尚未执行检测", "key-channels": "已读取 Key 当前的显式绑定渠道", "detect": "检测结束，原绑定已恢复；可手动应用通过渠道", "apply": "通过渠道已应用", "recover": "绑定恢复已完成"}[action]
		}
	}()
	return nil
}

func (s *hubManager) cancelJob() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		s.cancel()
		s.message = "正在取消，检测停止后恢复原绑定"
	}
}

func (s *hubManager) journal(recovery *hubRecovery) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.disk.Recovery = recovery
	return s.saveLocked()
}

func (s *hubManager) detect(ctx context.Context, client *hubClient, job *hubJob) (err error) {
	defer func() {
		// Manual cancellation stops probes first. Restoration has a separate
		// cancellation context; shutdown cancels both and leaves the journal intact.
		s.mu.Lock()
		recovery := s.disk.Recovery
		restoreCtx, restoreCancel := context.WithCancel(s.ctx)
		s.cancel, s.action = restoreCancel, "restore"
		s.mu.Unlock()
		if recovery != nil {
			if errRestore := s.recover(restoreCtx, client); errRestore != nil {
				err = errors.Join(err, fmt.Errorf("原绑定尚未恢复：%w", errRestore))
			}
		}
		restoreCancel()
		s.mu.Lock()
		defer s.mu.Unlock()
		job.Status, job.FinishedAt = "completed", time.Now()
		if err != nil {
			job.Status, job.Error = "failed", client.redact(err.Error())
		}
		if ctx.Err() != nil {
			job.Status = "cancelled"
			if job.Error == "" {
				job.Error = "检测已取消"
			}
		}
		for i := range job.Results {
			if job.Results[i].Result.Status == "queued" || job.Results[i].Result.Status == "running" {
				job.Results[i].Result.Status = "cancelled"
			}
		}
		if errSave := s.saveLocked(); errSave != nil {
			err = errors.Join(err, errSave)
		}
	}()
	key, errKey := client.key(ctx)
	if errKey != nil {
		return errKey
	}
	secret, errSecret := client.secret(ctx, key)
	if errSecret != nil {
		return errSecret
	}
	original, last := key.Profiles, key.Profiles
	for i, channel := range job.Results {
		if errContext := ctx.Err(); errContext != nil {
			return errContext
		}
		current, errCurrent := client.key(ctx)
		if errCurrent != nil {
			return errCurrent
		}
		if !equalHubProfiles(current.Profiles, last) {
			return fmt.Errorf("Key 绑定已被其他操作修改，检测已停止")
		}
		bound := original.bound([]int64{channel.Channel.ID})
		if errJournal := s.journal(&hubRecovery{Kind: "detect", JobID: job.ID, Original: original, Previous: last, Last: bound}); errJournal != nil {
			return errJournal
		}
		if errBind := client.setProfiles(ctx, bound); errBind != nil {
			return errBind
		}
		last = bound
		// Read back the exact binding before allowing any of the three probes.
		confirmed, errConfirmed := client.key(ctx)
		if errConfirmed != nil {
			return errConfirmed
		}
		if !equalHubProfiles(confirmed.Profiles, bound) {
			return fmt.Errorf("渠道绑定未生效，停止检测")
		}
		if errSettle := s.settle(ctx); errSettle != nil {
			return errSettle
		}
		progress := func(result evaluation.Result) {
			raw, _ := json.Marshal(result)
			var copyResult evaluation.Result
			_ = json.Unmarshal(raw, &copyResult)
			s.mu.Lock()
			job.Results[i].Result = copyResult
			s.mu.Unlock()
		}
		progress(evaluation.Result{Status: "running", Total: 3})
		result := s.run(ctx, client, secret, evaluation.Options{Model: job.Settings.Model, Effort: job.Settings.Effort, Test: "fingerprint"}, progress)
		progress(result)
		s.mu.Lock()
		errSave := s.saveLocked()
		s.mu.Unlock()
		if errSave != nil {
			return errSave
		}
	}
	return ctx.Err()
}

func (s *hubManager) apply(ctx context.Context, client *hubClient, job *hubJob) error {
	ids := []int64{}
	for _, channel := range job.Results {
		if hubPassed(channel.Result) {
			ids = append(ids, channel.Channel.ID)
		}
	}
	key, errKey := client.key(ctx)
	if errKey != nil {
		return errKey
	}
	bound := key.Profiles.bound(ids)
	if errJournal := s.journal(&hubRecovery{Kind: "apply", JobID: job.ID, Original: key.Profiles, Previous: key.Profiles, Last: bound}); errJournal != nil {
		return errJournal
	}
	if errBind := client.setProfiles(ctx, bound); errBind != nil {
		return errBind
	}
	return s.recover(ctx, client)
}

func (s *hubManager) recover(ctx context.Context, client *hubClient) error {
	s.mu.Lock()
	recovery := s.disk.Recovery
	s.mu.Unlock()
	if recovery == nil {
		return nil
	}
	key, errKey := client.key(ctx)
	if errKey != nil {
		return errKey
	}
	currentIsLast := equalHubProfiles(key.Profiles, recovery.Last)
	currentIsPrevious := equalHubProfiles(key.Profiles, recovery.Previous)
	if !currentIsLast && !currentIsPrevious && !equalHubProfiles(key.Profiles, recovery.Original) {
		return fmt.Errorf("Key 配置已被外部修改，未覆盖；请在 Hub 恢复原配置后再次点击恢复")
	}
	if recovery.Kind == "detect" && (currentIsLast || currentIsPrevious) && !equalHubProfiles(key.Profiles, recovery.Original) {
		if errRestore := client.setProfiles(ctx, recovery.Original); errRestore != nil {
			return errRestore
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if recovery.Kind == "apply" && currentIsLast {
		for _, job := range s.disk.Jobs {
			if job.ID == recovery.JobID {
				job.AppliedAt = time.Now()
			}
		}
	}
	s.disk.Recovery = nil
	if errSave := s.saveLocked(); errSave != nil {
		s.disk.Recovery = recovery
		return errSave
	}
	return nil
}
