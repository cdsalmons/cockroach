// Copyright 2015 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License. See the AUTHORS file
// for names of contributors.
//
// Author: Vivek Menezes (vivek@cockroachlabs.com)

package sql

import (
	"time"

	"github.com/cockroachdb/cockroach/util"
)

func (s Session) getLocation() (*time.Location, error) {
	switch t := s.Timezone.(type) {
	case nil:
		return time.UTC, nil
	case *Session_Location:
		//TODO(vivek): Cache the location.
		return time.LoadLocation(t.Location)
	case *Session_Offset:
		return time.FixedZone("", int(t.Offset)), nil
	default:
		return nil, util.Errorf("unhandled timezone variant type %T", t)
	}
}

func (m Session_Transaction_TableMutation) isSet() bool {
	return m.Name != ""
}
