// Copyright 2019 Yunion
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package idle

import (
	"encoding/json"
	"os"
	"time"

	"yunion.io/x/log"
)

// holidayFileJSON is the schema for --holiday-file.
// Prefer no new dependency: a simple JSON map of date → kind.
//
//	{
//	  "2026-01-01": "holiday",
//	  "2026-01-04": "workday"
//	}
//
// "holiday" = non-workday (including weekend holidays),
// "workday" = 调休 makeup workday (counts as workday even on weekend).
type holidayFileJSON map[string]string

type mapCalendar struct {
	days map[string]string
}

// loadHolidayCalendar returns the calendar plus a non-empty data_gaps entry
// when a configured file could not be used (also logged as a warning).
func loadHolidayCalendar(path string) (holidayCalendar, string) {
	if len(path) == 0 {
		return nil, ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		log.Warningf("cas idle: holiday file %q: %v (continuing without holidays)", path, err)
		return nil, "holiday_file=unreadable"
	}
	var raw holidayFileJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		log.Warningf("cas idle: holiday file %q: parse error: %v", path, err)
		return nil, "holiday_file=invalid"
	}
	return &mapCalendar{days: raw}, ""
}

func (c *mapCalendar) IsWorkday(d time.Time) (bool, bool) {
	if c == nil || c.days == nil {
		return false, false
	}
	key := d.Format("2006-01-02")
	v, ok := c.days[key]
	if !ok {
		return false, false
	}
	switch v {
	case "workday", "调休", "work":
		return true, true
	case "holiday", "rest", "off":
		return false, true
	default:
		return false, false
	}
}
