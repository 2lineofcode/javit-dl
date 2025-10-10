package jobs

import (
	"sync"
	"sync/atomic"
	"time"
)

type JobProgress struct {
	Phase             string    `json:"phase"`
	Percent           float64   `json:"percent"`
	Done              bool      `json:"done"`
	Error             string    `json:"error"`
	File              string    `json:"file"`
	TotalSegments     int       `json:"totalSegments"`
	CompletedSegments int32     `json:"completedSegments"`
	Started           time.Time `json:"started"`
}

var store sync.Map // map[string]*JobProgress

func SetJob(id string, p *JobProgress) { store.Store(id, p) }
func GetJob(id string) *JobProgress {
	if v, ok := store.Load(id); ok {
		return v.(*JobProgress)
	}
	return nil
}

func UpdateJobError(id, msg string) {
	if p := GetJob(id); p != nil {
		p.Error = msg
		p.Phase = "error"
	}
}
func UpdateJobDone(id, file string) {
	if p := GetJob(id); p != nil {
		p.Done = true
		p.Phase = "done"
		p.Percent = 100
		p.File = file
	}
}
func UpdateJobPhase(id, phase string) {
	if p := GetJob(id); p != nil {
		p.Phase = phase
	}
}
func UpdateJobSegmentsTotal(id string, total int) {
	if p := GetJob(id); p != nil {
		p.TotalSegments = total
		if total == 0 {
			p.Percent = 0
		}
	}
}
func IncrementJobSegment(id string) {
	if p := GetJob(id); p != nil {
		c := atomic.AddInt32(&p.CompletedSegments, 1)
		if p.TotalSegments > 0 {
			p.Percent = float64(c) / float64(p.TotalSegments) * 100
		}
	}
}
