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
	lmsClient      *lms.Client
	sendToCenter   SendToCenterFunc
	curriculum     *CurriculumData
	lastFetch      time.Time
	mu             sync.RWMutex
	triggerCh      chan struct{}
	log            *slog.Logger
}

func NewPoller(lmsClient *lms.Client) *Poller {
	return &Poller{
		lmsClient: lmsClient,
		triggerCh: make(chan struct{}, 1),
		log:       slog.With("component", "poller"),
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
					p.log.Error("Panic in poller", "panic", r)
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
		p.log.Error("Failed to get rollcalls", "error", err)
		return
	}
	p.log.Info("Polling", "active_rollcalls", len(rollcalls))

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
						p.log.Warn("Auto-radar: time match but course name differs",
							"curriculum_course", inst.Course, "rollcall_course", r.CourseTitle)
					}

					if inst.Location != "" {
						coords := GetLocationCoords(inst.Location)
						if coords != nil {
							p.log.Info("Auto-radar check-in", "course", inst.Course, "location", inst.Location)
							result := p.lmsClient.DoCheckin(ctx, r.RollcallID, "radar", map[string]interface{}{
								"lat": coords.Lat,
								"lon": coords.Lon,
							})
							if result.Success {
								p.log.Info("Auto-radar success", "course", r.CourseTitle)
							} else {
								p.log.Warn("Auto-radar failed", "course", r.CourseTitle, "error", result.ErrorCode)
							}
						} else {
							p.log.Warn("No coordinates for location", "location", inst.Location)
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
			{7*60 + 50, 12 * 60},       // 7:50-12:00
			{13*60 + 50, 18 * 60},      // 13:50-18:00
			{18*60 + 50, 22*60 + 40},   // 18:50-22:40
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

	p.log.Info("Fetching curriculum from API")

	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequestWithContext(ctx, "GET", config.Cfg.CurriculumAPI, nil)
	if err != nil {
		p.log.Error("Failed to create curriculum request", "error", err)
		return
	}

	resp, err := client.Do(req)
	if err != nil {
		p.log.Error("Failed to fetch curriculum", "error", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		p.log.Error("Curriculum API returned non-200", "status", resp.StatusCode)
		return
	}

	var data CurriculumData
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		p.log.Error("Failed to decode curriculum", "error", err)
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
		p.log.Warn("Failed to save curriculum cache", "error", err)
	}

	p.log.Info("Curriculum updated", "instances", len(data.Instances))
}

func (p *Poller) loadCurriculumFromFile() {
	data, err := os.ReadFile(config.CurriculumCachePath())
	if err != nil {
		return
	}

	var cache curriculumCache
	if err := json.Unmarshal(data, &cache); err != nil {
		p.log.Warn("Failed to parse curriculum cache", "error", err)
		return
	}

	p.mu.Lock()
	p.curriculum = &cache.Data
	if t, err := time.Parse(time.RFC3339, cache.UpdatedAt); err == nil {
		p.lastFetch = t
	}
	p.mu.Unlock()

	p.log.Info("Loaded curriculum from cache")
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
