package center

import (
	"sync"
	"time"
)

type NumberTask struct {
	CourseTitle     string  `json:"course_title"`
	CourseLocation  *string `json:"course_location"`
	RollcallNumber *int    `json:"rollcall_number"`
	UpdatedAt      float64 `json:"updated_at"`
}

type SharedState struct {
	mu sync.RWMutex

	// QR state
	latestQRData      string
	latestQRTimestamp int64
	qrSuccessClients  map[string]struct{}
	qrNeedingClients  map[string]struct{}

	// Number state
	numberTasks map[int]*NumberTask // rollcall_id -> task
}

func NewSharedState() *SharedState {
	return &SharedState{
		qrSuccessClients: make(map[string]struct{}),
		qrNeedingClients: make(map[string]struct{}),
		numberTasks:      make(map[int]*NumberTask),
	}
}

// IsQRValid checks if the QR data is valid (within 15 seconds).
func (s *SharedState) IsQRValid() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.isQRValidLocked()
}

func (s *SharedState) isQRValidLocked() bool {
	if s.latestQRData == "" || len(s.latestQRData) < 10 {
		return false
	}
	return (time.Now().Unix() - s.latestQRTimestamp) <= 15
}

// parseQRTimestamp extracts the 10-digit unix timestamp from a QR string.
// Returns -1 if invalid.
func parseQRTimestamp(qrString string) int64 {
	if len(qrString) < 10 {
		return -1
	}
	var ts int64
	for i := 0; i < 10; i++ {
		c := qrString[i]
		if c < '0' || c > '9' {
			return -1
		}
		ts = ts*10 + int64(c-'0')
	}
	return ts
}

// UpdateQRData updates the QR data if it's newer and valid. Returns true if updated.
func (s *SharedState) UpdateQRData(qrString string) bool {
	ts := parseQRTimestamp(qrString)
	if ts < 0 || (time.Now().Unix()-ts) > 15 {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if ts > s.latestQRTimestamp {
		s.latestQRTimestamp = ts
		s.latestQRData = qrString
		s.qrSuccessClients = make(map[string]struct{}) // Reset on new QR
		return true
	}
	return false
}

// MatchesCurrentQR returns true if qrString equals the current latest QR data.
func (s *SharedState) MatchesCurrentQR(qrString string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return qrString == s.latestQRData
}

// SetQRNeedingClient updates whether a client needs QR check-in.
// When transitioning to needing, resets success state.
// When transitioning to not-needing, removes from both sets.
func (s *SharedState) SetQRNeedingClient(clientID string, needs bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if needs {
		if _, already := s.qrNeedingClients[clientID]; !already {
			delete(s.qrSuccessClients, clientID)
		}
		s.qrNeedingClients[clientID] = struct{}{}
	} else {
		delete(s.qrNeedingClients, clientID)
		delete(s.qrSuccessClients, clientID)
	}
}

func (s *SharedState) AddQRSuccessClient(clientID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.qrSuccessClients[clientID] = struct{}{}
}

func (s *SharedState) RemoveClient(clientID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.qrNeedingClients, clientID)
	delete(s.qrSuccessClients, clientID)
}

func (s *SharedState) GetCurrentQR() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.isQRValidLocked() {
		return s.latestQRData
	}
	return ""
}

func (s *SharedState) GetRemainingSeconds() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.isQRValidLocked() {
		return 0
	}
	remaining := 15 - int(time.Now().Unix()-s.latestQRTimestamp)
	if remaining < 0 {
		return 0
	}
	return remaining
}

func (s *SharedState) UncheckinCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	count := 0
	for id := range s.qrNeedingClients {
		if _, ok := s.qrSuccessClients[id]; !ok {
			count++
		}
	}
	return count
}

// GetOrCreateNumberTask returns the number task for a rollcall, creating it if needed.
func (s *SharedState) GetOrCreateNumberTask(rollcallID int, courseTitle string, courseLocation *string) *NumberTask {
	s.mu.Lock()
	defer s.mu.Unlock()

	task, exists := s.numberTasks[rollcallID]
	if !exists {
		task = &NumberTask{
			CourseTitle:    courseTitle,
			CourseLocation: courseLocation,
			UpdatedAt:      float64(time.Now().Unix()),
		}
		s.numberTasks[rollcallID] = task
	} else {
		task.CourseTitle = courseTitle
		task.CourseLocation = courseLocation
		task.UpdatedAt = float64(time.Now().Unix())
	}
	return task
}

// UpdateNumberTaskAnswer sets the number code for a rollcall task.
func (s *SharedState) UpdateNumberTaskAnswer(rollcallID int, number int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	task, exists := s.numberTasks[rollcallID]
	if exists {
		task.RollcallNumber = &number
		task.UpdatedAt = float64(time.Now().Unix())
	} else {
		s.numberTasks[rollcallID] = &NumberTask{
			RollcallNumber: &number,
			UpdatedAt:      float64(time.Now().Unix()),
		}
	}
}

// GetNumberTask returns the number task if it exists and has a valid number (within 24h).
func (s *SharedState) GetNumberTask(rollcallID int) *NumberTask {
	s.mu.RLock()
	defer s.mu.RUnlock()

	task, exists := s.numberTasks[rollcallID]
	if !exists || task.RollcallNumber == nil {
		return nil
	}

	// Check 24h expiry
	if (float64(time.Now().Unix()) - task.UpdatedAt) > 86400 {
		return nil
	}

	return task
}
