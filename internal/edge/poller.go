package edge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/Auto-CQUPT-Plan/rollcall-go/internal/config"
	"github.com/Auto-CQUPT-Plan/rollcall-go/internal/lms"
	"github.com/Auto-CQUPT-Plan/rollcall-go/internal/notify"
)

type CurriculumInstance struct {
	Date      string `json:"date"`
	StartTime string `json:"start_time"`
	EndTime   string `json:"end_time"`
	Course    string `json:"course"`
	Location  string `json:"location"`
}

type CurriculumData struct {
	Instances []CurriculumInstance `json:"instances"`
}

type curriculumCache struct {
	UpdatedAt string         `json:"_updated_at"`
	Data      CurriculumData `json:"data"`
}

// SendToCenterFunc is the function used to send messages to the center server.
// Set by the WSClient after initialization.
type SendToCenterFunc func(msg map[string]interface{})

type Poller struct {
	lmsClient          *lms.Client
	sendToCenter       SendToCenterFunc
	curriculum         *CurriculumData
	lastFetch          time.Time
	mu                 sync.RWMutex
	triggerCh          chan struct{}
	log                *slog.Logger
	activeRollcalls    map[int]lms.Rollcall // 用于跟踪活跃签到，避免重复打印
	completedRollcalls map[int]bool         // 用于跟踪已完成签到，避免重复打印
	pollLogCache       map[string]int       // 用于缓存重复日志的计数
}

func NewPoller(lmsClient *lms.Client) *Poller {
	return &Poller{
		lmsClient:          lmsClient,
		triggerCh:          make(chan struct{}, 1),
		log:                slog.With("component", "poller"),
		activeRollcalls:    make(map[int]lms.Rollcall),
		completedRollcalls: make(map[int]bool),
		pollLogCache:       make(map[string]int),
	}
}

// SetSendFunc sets the function used to send messages to the center server.
func (p *Poller) SetSendFunc(fn SendToCenterFunc) {
	p.sendToCenter = fn
}

// TriggerPoll wakes up the polling loop immediately.
func (p *Poller) TriggerPoll() {
	select {
	case p.triggerCh <- struct{}{}:
	default:
	}
}

// Run starts the polling loop. Blocks until ctx is cancelled.
func (p *Poller) Run(ctx context.Context) {
	p.loadCurriculumFromFile()

	for {
		func() {
			defer func() {
				if r := recover(); r != nil {
					p.log.Error("轮询器异常", "panic", r)
				}
			}()
			p.pollOnce(ctx)
		}()

		select {
		case <-ctx.Done():
			return
		case <-p.triggerCh:
		case <-time.After(30 * time.Second):
		}
	}
}

func (p *Poller) pollOnce(ctx context.Context) {
	// Update curriculum if needed
	if config.Cfg.CurriculumAPI != "" {
		p.fetchCurriculum(ctx)
	}

	if !p.shouldPoll() {
		return
	}

	rollcalls, err := p.lmsClient.GetRollcalls(ctx)
	if err != nil {
		logKey := "get_rollcalls_error:" + err.Error()
		p.mu.Lock()
		p.pollLogCache[logKey]++
		count := p.pollLogCache[logKey]
		p.mu.Unlock()
		if count == 1 {
			p.log.Error("获取签到列表失败", "error", err)
		} else {
			p.log.Error("获取签到列表失败", "error", err, "Count", count)
		}
		return
	}

	// 处理活跃签到，只打印一次
	p.mu.Lock()
	// 检查是否有新的活跃签到
	newActiveRollcalls := make(map[int]lms.Rollcall)
	for _, r := range rollcalls {
		newActiveRollcalls[r.RollcallID] = r
		if _, exists := p.activeRollcalls[r.RollcallID]; !exists && r.Status == "absent" {
			p.log.Info("发现活跃签到", "rollcall_id", r.RollcallID, "source", r.Source, "course", r.CourseTitle)
			notify.Sendf("🔍 发现活跃签到\n课程: %s\n类型: %s", r.CourseTitle, r.Source)
		}
	}
	// 检查是否有签到完成
	for id, r := range p.activeRollcalls {
		if newR, exists := newActiveRollcalls[id]; exists && newR.Status != "absent" && !p.completedRollcalls[id] {
			p.completedRollcalls[id] = true
			p.log.Info("签到完成", "rollcall_id", id, "source", r.Source, "course", r.CourseTitle)
			notify.Sendf("✅ 签到完成\n课程: %s\n类型: %s", r.CourseTitle, r.Source)
		}
	}
	// 更新活跃签到列表
	p.activeRollcalls = newActiveRollcalls
	p.mu.Unlock()

	// Build rollcall_tasks message for center
	hasQR := false
	var numbers []map[string]interface{}
	for _, r := range rollcalls {
		if r.Status != "absent" {
			continue
		}
		switch r.Source {
		case "qr":
			hasQR = true
		case "number":
			numbers = append(numbers, map[string]interface{}{
				"rollcall_id":     r.RollcallID,
				"course_title":    r.CourseTitle,
				"course_location": p.getCourseLocationForRollcall(r),
			})
		}
	}

	if p.sendToCenter != nil {
		p.sendToCenter(map[string]interface{}{
			"type":            "rollcall_tasks",
			"client_id":       config.ClientID,
			"rollcall_qr":     hasQR,
			"rollcall_number": numbers,
			"timestamp":       time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		})
	}

	// Auto radar check-in
	if config.Cfg.CurriculumAPI != "" && config.Cfg.AutoLocationCheckin {
		inst := p.getCurrentCourseInstance(time.Now())
		if inst != nil {
			for _, r := range rollcalls {
				if r.Source == "radar" && r.Status == "absent" {
					if r.CourseTitle != inst.Course {
						p.log.Warn("自动定位: 时间匹配但课程名不同",
							"课表课程", inst.Course, "签到课程", r.CourseTitle)
					}

					if inst.Location != "" {
						coords := GetLocationCoords(inst.Location)
						if coords != nil {
							p.log.Info("自动定位签到中", "课程", inst.Course, "地点", inst.Location)
							result := p.lmsClient.DoCheckin(ctx, r.RollcallID, "radar", map[string]interface{}{
								"lat": coords.Lat,
								"lon": coords.Lon,
							})
							if result.Success {
								p.log.Info("自动定位签到成功", "课程", r.CourseTitle)
								notify.Sendf("✅ 自动定位签到成功\n课程: %s\n地点: %s", r.CourseTitle, inst.Location)
							} else {
								logKey := fmt.Sprintf("radar_checkin_failed:%d:%s", r.RollcallID, result.ErrorCode)
								p.mu.Lock()
								p.pollLogCache[logKey]++
								count := p.pollLogCache[logKey]
								p.mu.Unlock()
								if count == 1 {
									p.log.Warn("自动定位签到失败", "课程", r.CourseTitle, "error", result.ErrorCode)
									notify.Sendf("❌ 自动定位签到失败\n课程: %s\n原因: %s", r.CourseTitle, result.ErrorCode)
								} else {
									p.log.Warn("自动定位签到失败", "课程", r.CourseTitle, "error", result.ErrorCode, "Count", count)
								}
							}
						} else {
							logKey := "location_coords_not_found:" + inst.Location
							p.mu.Lock()
							p.pollLogCache[logKey]++
							count := p.pollLogCache[logKey]
							p.mu.Unlock()
							if count == 1 {
								p.log.Warn("未找到地点坐标", "地点", inst.Location)
							} else {
								p.log.Warn("未找到地点坐标", "地点", inst.Location, "Count", count)
							}
						}
					}
				}
			}
		}
	}

	// Auto number check-in
	if config.Cfg.AutoNumberCheckin {
		for _, r := range rollcalls {
			if r.Source == "number" && r.Status == "absent" {
				p.log.Info("自动数字签到: 发现未完成任务", "rollcall_id", r.RollcallID, "course", r.CourseTitle)
				studentData, err := p.lmsClient.GetStudentRollcalls(ctx, r.RollcallID)
				if err != nil {
					logKey := fmt.Sprintf("get_student_rollcalls_error:%d:%s", r.RollcallID, err.Error())
					p.mu.Lock()
					p.pollLogCache[logKey]++
					count := p.pollLogCache[logKey]
					p.mu.Unlock()
					if count == 1 {
						p.log.Error("获取学生签到详情失败", "rollcall_id", r.RollcallID, "error", err)
					} else {
						p.log.Error("获取学生签到详情失败", "rollcall_id", r.RollcallID, "error", err, "Count", count)
					}
					continue
				}
				if studentData != nil {
					// 计算已签到人数
					checkedInCount := 0
					for _, sr := range studentData.RollcallList {
						if sr.Status == "on_call" {
							checkedInCount++
						}
					}
					p.log.Info("自动数字签到: 检查任务状态", "rollcall_id", r.RollcallID,
						"is_number", studentData.IsNumber, "number_code", studentData.NumberCode,
						"checked_in_count", checkedInCount)
					if studentData.IsNumber && studentData.NumberCode > 0 && checkedInCount > 0 {
						p.log.Info("自动数字签到: 发现有人已签到，提交签到码",
							"rollcall_id", r.RollcallID, "number_code", studentData.NumberCode)
						result := p.lmsClient.DoCheckin(ctx, r.RollcallID, "number", map[string]interface{}{
							"numberCode": fmt.Sprintf("%d", studentData.NumberCode),
						})
						if result.Success {
							p.log.Info("自动数字签到成功", "课程", r.CourseTitle, "rollcall_id", r.RollcallID)
							notify.Sendf("✅ 自动数字签到成功\n课程: %s\n签到码: %d", r.CourseTitle, studentData.NumberCode)
							// 发送成功信息到中心服务器
							if p.sendToCenter != nil {
								courseLocation := p.getCourseLocationForRollcall(r)
								p.sendToCenter(map[string]interface{}{
									"type":            "rollcall_success",
									"client_id":       config.ClientID,
									"rollcall_type":   "number",
									"course_title":    r.CourseTitle,
									"course_location": courseLocation,
									"rollcall_id":     r.RollcallID,
									"rollcall_number": studentData.NumberCode,
									"timestamp":       time.Now().UTC().Format("2006-01-02T15:04:05Z"),
								})
							}
							p.TriggerPoll()
						} else {
							logKey := fmt.Sprintf("number_checkin_failed:%d:%s", r.RollcallID, result.ErrorCode)
							p.mu.Lock()
							p.pollLogCache[logKey]++
							count := p.pollLogCache[logKey]
							p.mu.Unlock()
							if count == 1 {
								p.log.Warn("自动数字签到失败", "课程", r.CourseTitle, "error", result.ErrorCode)
								notify.Sendf("❌ 自动数字签到失败\n课程: %s\n原因: %s", r.CourseTitle, result.ErrorCode)
							} else {
								p.log.Warn("自动数字签到失败", "课程", r.CourseTitle, "error", result.ErrorCode, "Count", count)
							}
						}
					}
				}
			}
		}
	}
}

func (p *Poller) shouldPoll() bool {
	now := time.Now()
	nowTime := now.Hour()*60 + now.Minute()

	if config.Cfg.CurriculumAPI == "" {
		// Default windows
		windows := [][2]int{
			{7*60 + 50, 12 * 60},     // 7:50-12:00
			{13*60 + 50, 18 * 60},    // 13:50-18:00
			{18*60 + 50, 22*60 + 40}, // 18:50-22:40
		}
		for _, w := range windows {
			if nowTime >= w[0] && nowTime <= w[1] {
				return true
			}
		}
		return false
	}

	p.mu.RLock()
	curriculum := p.curriculum
	p.mu.RUnlock()

	if curriculum == nil {
		return true // Default to poll if no data
	}

	todayStr := now.Format("2006-01-02")
	for _, inst := range curriculum.Instances {
		if inst.Date != todayStr {
			continue
		}
		startDT, endDT, err := parseTimeRange(todayStr, inst.StartTime, inst.EndTime)
		if err != nil {
			continue
		}
		pollStart := startDT.Add(-time.Duration(config.Cfg.CurriculumPreMinutes) * time.Minute)
		if now.After(pollStart) && now.Before(endDT) {
			return true
		}
	}

	return false
}

func (p *Poller) getCurrentCourseInstance(checkTime time.Time) *CurriculumInstance {
	p.mu.RLock()
	curriculum := p.curriculum
	p.mu.RUnlock()

	if curriculum == nil {
		return nil
	}

	todayStr := checkTime.Format("2006-01-02")
	for _, inst := range curriculum.Instances {
		if inst.Date != todayStr {
			continue
		}
		startDT, endDT, err := parseTimeRange(todayStr, inst.StartTime, inst.EndTime)
		if err != nil {
			continue
		}
		// 15 min buffer before start
		if checkTime.After(startDT.Add(-15*time.Minute)) && checkTime.Before(endDT) {
			return &inst
		}
	}
	return nil
}

func (p *Poller) getCourseLocationForRollcall(r lms.Rollcall) interface{} {
	rtStr := r.RollcallTime
	if rtStr == "" {
		return nil
	}

	var rtUTC time.Time
	var err error
	if len(rtStr) > 0 && rtStr[len(rtStr)-1] == 'Z' {
		rtUTC, err = time.Parse("2006-01-02T15:04:05Z", rtStr)
	} else {
		rtUTC, err = time.Parse(time.RFC3339, rtStr)
	}
	if err != nil {
		return nil
	}

	// Convert to UTC+8
	loc := time.FixedZone("UTC+8", 8*3600)
	rtLocal := rtUTC.In(loc)

	inst := p.getCurrentCourseInstance(rtLocal)
	if inst != nil {
		return inst.Location
	}
	return nil
}

func (p *Poller) fetchCurriculum(ctx context.Context) {
	p.mu.RLock()
	lastFetch := p.lastFetch
	p.mu.RUnlock()

	if !lastFetch.IsZero() && time.Since(lastFetch) < 30*time.Minute {
		return
	}

	p.log.Info("正在获取课表")

	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequestWithContext(ctx, "GET", config.Cfg.CurriculumAPI, nil)
	if err != nil {
		p.log.Error("创建课表请求失败", "error", err)
		return
	}

	resp, err := client.Do(req)
	if err != nil {
		p.log.Error("获取课表失败", "error", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		p.log.Error("课表 API 返回异常", "状态码", resp.StatusCode)
		return
	}

	var data CurriculumData
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		p.log.Error("课表解析失败", "error", err)
		return
	}

	p.mu.Lock()
	p.curriculum = &data
	p.lastFetch = time.Now()
	p.mu.Unlock()

	// Save to cache file
	cache := curriculumCache{
		UpdatedAt: time.Now().Format(time.RFC3339),
		Data:      data,
	}
	cacheData, _ := json.MarshalIndent(cache, "", "  ")
	if err := os.WriteFile(config.CurriculumCachePath(), cacheData, 0o644); err != nil {
		p.log.Warn("课表缓存保存失败", "error", err)
	}

	p.log.Info("课表已更新", "课程数", len(data.Instances))
}

func (p *Poller) loadCurriculumFromFile() {
	data, err := os.ReadFile(config.CurriculumCachePath())
	if err != nil {
		return
	}

	var cache curriculumCache
	if err := json.Unmarshal(data, &cache); err != nil {
		p.log.Warn("课表缓存解析失败", "error", err)
		return
	}

	p.mu.Lock()
	p.curriculum = &cache.Data
	if t, err := time.Parse(time.RFC3339, cache.UpdatedAt); err == nil {
		p.lastFetch = t
	}
	p.mu.Unlock()

	p.log.Info("已从缓存加载课表")
}

func parseTimeRange(dateStr, startStr, endStr string) (time.Time, time.Time, error) {
	layout := "2006-01-02 15:04"
	startDT, err := time.ParseInLocation(layout, fmt.Sprintf("%s %s", dateStr, startStr), time.Local)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	endDT, err := time.ParseInLocation(layout, fmt.Sprintf("%s %s", dateStr, endStr), time.Local)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	return startDT, endDT, nil
}
