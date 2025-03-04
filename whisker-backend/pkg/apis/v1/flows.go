// Copyright (c) 2025 Tigera, Inc. All rights reserved.
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

package v1

import (
	"fmt"
	"time"

	"github.com/projectcalico/calico/lib/httpmachinery/pkg/codec"
)

const (
	sep = "/"

	FlowsPath = sep + "flows"
)

func init() {
	// Register a decoder for the listFlowsSortBy.
	codec.RegisterCustomDecodeTypeFunc(func(vals []string) ([]ListFlowsSortBy, error) {
		var values []ListFlowsSortBy
		for _, v := range vals {
			switch ListFlowsSortBy(v) {
			case ListFlowsSortByDefault, ListFlowsSortByDestName, ListFlowsSortBySrcName:
				values = append(values, ListFlowsSortBy(v))
			default:
				return nil, fmt.Errorf("unknown sortBy value: %s", vals[0])
			}
		}
		return values, nil
	})

	codec.RegisterURLQueryJSONType[Filters]()
}

// ListFlowsSortBy represents the different values you can use to sort by in the list flows API. It is unexported so that
// strings cannot be cast as this type external to this package, ensuring that users cannot set invalid sort by parameters
// to the API structs using this type.
//
// The decode function registered in the init function ensures that any string that would be decoded into this type is
// allowed, and fails to decode for invalid values.
type ListFlowsSortBy string

const (
	ListFlowsSortByDefault  ListFlowsSortBy = ""
	ListFlowsSortBySrcName  ListFlowsSortBy = "source_name"
	ListFlowsSortByDestName ListFlowsSortBy = "dest_name"
)

type ListFlowsParams struct {
	Watch       bool              `urlQuery:"watch"`
	StartTimeGt int64             `urlQuery:"startTimeGt"`
	StartTimeLt int64             `urlQuery:"startTimeLt"`
	SortBy      []ListFlowsSortBy `urlQuery:"sortBy"`

	Filters Filters `urlQuery:"filters"`
}

type Filters struct {
	SourceName      string `json:"source_name,omitempty"`
	SourceNamespace string `json:"source_namespace,omitempty"`
	SourceLabels    string `json:"source_labels,omitempty"`
	DestName        string `json:"dest_name,omitempty"`
	DestNamespace   string `json:"dest_namespace,omitempty"`
	DestLabels      string `json:"dest_labels,omitempty"`
	Protocol        string `json:"protocol,omitempty"`
	DestPort        int64  `json:"dest_port,omitempty"`
	Action          string `json:"action,omitempty"`
}

type StreamFlowsParams struct {
	Filters Filters `urlQuery:"filters"`
}

type FlowResponse struct {
	StartTime       time.Time `json:"start_time"`
	EndTime         time.Time `json:"end_time"`
	Action          string    `json:"action"`
	SourceName      string    `json:"source_name"`
	SourceNamespace string    `json:"source_namespace"`
	SourceLabels    string    `json:"source_labels"`
	DestName        string    `json:"dest_name"`
	DestNamespace   string    `json:"dest_namespace"`
	DestLabels      string    `json:"dest_labels"`
	Protocol        string    `json:"protocol"`
	DestPort        int64     `json:"dest_port"`
	Reporter        string    `json:"reporter"`
	PacketsIn       int64     `json:"packets_in"`
	PacketsOut      int64     `json:"packets_out"`
	BytesIn         int64     `json:"bytes_in"`
	BytesOut        int64     `json:"bytes_out"`
}
