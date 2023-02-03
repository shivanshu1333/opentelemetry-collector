// Copyright The OpenTelemetry Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//       http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gonum.org/v1/gonum/graph/simple"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/connector"
	"go.opentelemetry.io/collector/connector/connectortest"
	"go.opentelemetry.io/collector/exporter"
	"go.opentelemetry.io/collector/exporter/exportertest"
	"go.opentelemetry.io/collector/internal/testdata"
	"go.opentelemetry.io/collector/processor"
	"go.opentelemetry.io/collector/processor/processortest"
	"go.opentelemetry.io/collector/receiver"
	"go.opentelemetry.io/collector/receiver/receivertest"
	"go.opentelemetry.io/collector/service/internal/testcomponents"
)

var _ component.Component = &testNode{}

type testNode struct {
	id          component.ID
	startErr    error
	shutdownErr error
}

// ID satisfies the graph.Node interface, allowing
// testNode to be used in a simple.DirectedGraph
func (n *testNode) ID() int64 {
	return int64(newNodeID(n.id.String()))
}

func (n *testNode) Start(ctx context.Context, _ component.Host) error {
	if n.startErr != nil {
		return n.startErr
	}
	if cwo, ok := ctx.(*contextWithOrder); ok {
		cwo.record(n.id)
	}
	return nil
}

func (n *testNode) Shutdown(ctx context.Context) error {
	if n.shutdownErr != nil {
		return n.shutdownErr
	}
	if cwo, ok := ctx.(*contextWithOrder); ok {
		cwo.record(n.id)
	}
	return nil
}

type contextWithOrder struct {
	context.Context
	sync.Mutex
	next  int
	order map[component.ID]int
}

func (c *contextWithOrder) record(id component.ID) {
	c.Lock()
	c.order[id] = c.next
	c.next++
	c.Unlock()
}

func TestGraphStartStop(t *testing.T) {
	testCases := []struct {
		name  string
		edges [][2]component.ID
	}{
		{
			name: "single",
			edges: [][2]component.ID{
				{component.NewIDWithName("r", "1"), component.NewIDWithName("p", "1")},
				{component.NewIDWithName("r", "2"), component.NewIDWithName("p", "1")},
				{component.NewIDWithName("p", "1"), component.NewIDWithName("p", "2")},
				{component.NewIDWithName("p", "2"), component.NewIDWithName("e", "1")},
				{component.NewIDWithName("p", "1"), component.NewIDWithName("e", "2")},
			},
		},
		{
			name: "multi",
			edges: [][2]component.ID{
				// Pipeline 1
				{component.NewIDWithName("r", "1"), component.NewIDWithName("p", "1")},
				{component.NewIDWithName("r", "2"), component.NewIDWithName("p", "1")},
				{component.NewIDWithName("p", "1"), component.NewIDWithName("p", "2")},
				{component.NewIDWithName("p", "2"), component.NewIDWithName("e", "1")},
				{component.NewIDWithName("p", "1"), component.NewIDWithName("e", "2")},

				// Pipeline 2, shares r1 and e2
				{component.NewIDWithName("r", "1"), component.NewIDWithName("p", "3")},
				{component.NewIDWithName("p", "3"), component.NewIDWithName("e", "2")},
			},
		},
		{
			name: "connected",
			edges: [][2]component.ID{
				// Pipeline 1
				{component.NewIDWithName("r", "1"), component.NewIDWithName("p", "1")},
				{component.NewIDWithName("r", "2"), component.NewIDWithName("p", "1")},
				{component.NewIDWithName("p", "1"), component.NewIDWithName("p", "2")},
				{component.NewIDWithName("p", "2"), component.NewIDWithName("e", "1")},
				{component.NewIDWithName("p", "1"), component.NewIDWithName("c", "1")},

				// Pipeline 2, shares r1 and c1
				{component.NewIDWithName("r", "1"), component.NewIDWithName("p", "3")},
				{component.NewIDWithName("p", "3"), component.NewIDWithName("c", "1")},

				// Pipeline 3, emits to e2 and c2
				{component.NewIDWithName("c", "1"), component.NewIDWithName("e", "2")},
				{component.NewIDWithName("c", "1"), component.NewIDWithName("c", "2")},

				// Pipeline 4, also emits to e2
				{component.NewIDWithName("c", "2"), component.NewIDWithName("e", "2")},
			},
		},
	}

	for _, tt := range testCases {
		t.Run(tt.name, func(t *testing.T) {
			ctx := &contextWithOrder{
				Context: context.Background(),
				order:   map[component.ID]int{},
			}

			pg := &pipelinesGraph{componentGraph: simple.NewDirectedGraph()}
			for _, edge := range tt.edges {
				f, t := &testNode{id: edge[0]}, &testNode{id: edge[1]}
				pg.componentGraph.SetEdge(simple.Edge{F: f, T: t})
			}

			require.NoError(t, pg.StartAll(ctx, componenttest.NewNopHost()))
			for _, edge := range tt.edges {
				assert.Greater(t, ctx.order[edge[0]], ctx.order[edge[1]])
			}

			ctx.order = map[component.ID]int{}
			require.NoError(t, pg.ShutdownAll(ctx))
			for _, edge := range tt.edges {
				assert.Less(t, ctx.order[edge[0]], ctx.order[edge[1]])
			}
		})
	}
}

func TestGraphStartStopCycle(t *testing.T) {
	pg := &pipelinesGraph{componentGraph: simple.NewDirectedGraph()}

	r1 := &testNode{id: component.NewIDWithName("r", "1")}
	p1 := &testNode{id: component.NewIDWithName("p", "1")}
	c1 := &testNode{id: component.NewIDWithName("c", "1")}
	e1 := &testNode{id: component.NewIDWithName("e", "1")}

	pg.componentGraph.SetEdge(simple.Edge{F: r1, T: p1})
	pg.componentGraph.SetEdge(simple.Edge{F: p1, T: c1})
	pg.componentGraph.SetEdge(simple.Edge{F: c1, T: e1})
	pg.componentGraph.SetEdge(simple.Edge{F: c1, T: p1}) // loop back

	err := pg.StartAll(context.Background(), componenttest.NewNopHost())
	assert.Error(t, err)
	assert.Contains(t, err.Error(), `topo: no topological ordering: cyclic components`)

	err = pg.ShutdownAll(context.Background())
	assert.Error(t, err)
	assert.Contains(t, err.Error(), `topo: no topological ordering: cyclic components`)
}

func TestGraphStartStopComponentError(t *testing.T) {
	pg := &pipelinesGraph{componentGraph: simple.NewDirectedGraph()}
	pg.componentGraph.SetEdge(simple.Edge{
		F: &testNode{
			id:       component.NewIDWithName("r", "1"),
			startErr: errors.New("foo"),
		},
		T: &testNode{
			id:          component.NewIDWithName("e", "1"),
			shutdownErr: errors.New("bar"),
		},
	})
	assert.EqualError(t, pg.StartAll(context.Background(), componenttest.NewNopHost()), "foo")
	assert.EqualError(t, pg.ShutdownAll(context.Background()), "bar")
}

func TestConnectorPipelinesGraph(t *testing.T) {
	tests := []struct {
		name                string
		pipelineConfigs     map[component.ID]*PipelineConfig
		expectedPerExporter int // requires symmetry in pipelines
	}{
		{
			name: "pipelines_simple.yaml",
			pipelineConfigs: map[component.ID]*PipelineConfig{
				component.NewID("traces"): {
					Receivers:  []component.ID{component.NewID("examplereceiver")},
					Processors: []component.ID{component.NewID("exampleprocessor")},
					Exporters:  []component.ID{component.NewID("exampleexporter")},
				},
				component.NewID("metrics"): {
					Receivers:  []component.ID{component.NewID("examplereceiver")},
					Processors: []component.ID{component.NewID("exampleprocessor")},
					Exporters:  []component.ID{component.NewID("exampleexporter")},
				},
				component.NewID("logs"): {
					Receivers:  []component.ID{component.NewID("examplereceiver")},
					Processors: []component.ID{component.NewID("exampleprocessor")},
					Exporters:  []component.ID{component.NewID("exampleexporter")},
				},
			},
			expectedPerExporter: 1,
		},
		{
			name: "pipelines_simple_mutate.yaml",
			pipelineConfigs: map[component.ID]*PipelineConfig{
				component.NewID("traces"): {
					Receivers:  []component.ID{component.NewID("examplereceiver")},
					Processors: []component.ID{component.NewIDWithName("exampleprocessor", "mutate")},
					Exporters:  []component.ID{component.NewID("exampleexporter")},
				},
				component.NewID("metrics"): {
					Receivers:  []component.ID{component.NewID("examplereceiver")},
					Processors: []component.ID{component.NewIDWithName("exampleprocessor", "mutate")},
					Exporters:  []component.ID{component.NewID("exampleexporter")},
				},
				component.NewID("logs"): {
					Receivers:  []component.ID{component.NewID("examplereceiver")},
					Processors: []component.ID{component.NewIDWithName("exampleprocessor", "mutate")},
					Exporters:  []component.ID{component.NewID("exampleexporter")},
				},
			},
			expectedPerExporter: 1,
		},
		{
			name: "pipelines_simple_multi_proc.yaml",
			pipelineConfigs: map[component.ID]*PipelineConfig{
				component.NewID("traces"): {
					Receivers:  []component.ID{component.NewID("examplereceiver")},
					Processors: []component.ID{component.NewID("exampleprocessor"), component.NewIDWithName("exampleprocessor", "mutate")},
					Exporters:  []component.ID{component.NewID("exampleexporter")},
				},
				component.NewID("metrics"): {
					Receivers:  []component.ID{component.NewID("examplereceiver")},
					Processors: []component.ID{component.NewID("exampleprocessor"), component.NewIDWithName("exampleprocessor", "mutate")},
					Exporters:  []component.ID{component.NewID("exampleexporter")},
				},
				component.NewID("logs"): {
					Receivers:  []component.ID{component.NewID("examplereceiver")},
					Processors: []component.ID{component.NewID("exampleprocessor"), component.NewIDWithName("exampleprocessor", "mutate")},
					Exporters:  []component.ID{component.NewID("exampleexporter")},
				},
			},
			expectedPerExporter: 1,
		},
		{
			name: "pipelines_simple_no_proc.yaml",
			pipelineConfigs: map[component.ID]*PipelineConfig{
				component.NewID("traces"): {
					Receivers: []component.ID{component.NewID("examplereceiver")},
					Exporters: []component.ID{component.NewID("exampleexporter")},
				},
				component.NewID("metrics"): {
					Receivers: []component.ID{component.NewID("examplereceiver")},
					Exporters: []component.ID{component.NewID("exampleexporter")},
				},
				component.NewID("logs"): {
					Receivers: []component.ID{component.NewID("examplereceiver")},
					Exporters: []component.ID{component.NewID("exampleexporter")},
				},
			},
			expectedPerExporter: 1,
		},
		{
			name: "pipelines_multi.yaml",
			pipelineConfigs: map[component.ID]*PipelineConfig{
				component.NewID("traces"): {
					Receivers:  []component.ID{component.NewID("examplereceiver"), component.NewIDWithName("examplereceiver", "1")},
					Processors: []component.ID{component.NewIDWithName("exampleprocessor", "mutate"), component.NewID("exampleprocessor")},
					Exporters:  []component.ID{component.NewID("exampleexporter"), component.NewIDWithName("exampleexporter", "1")},
				},
				component.NewID("metrics"): {
					Receivers:  []component.ID{component.NewID("examplereceiver"), component.NewIDWithName("examplereceiver", "1")},
					Processors: []component.ID{component.NewIDWithName("exampleprocessor", "mutate"), component.NewID("exampleprocessor")},
					Exporters:  []component.ID{component.NewID("exampleexporter"), component.NewIDWithName("exampleexporter", "1")},
				},
				component.NewID("logs"): {
					Receivers:  []component.ID{component.NewID("examplereceiver"), component.NewIDWithName("examplereceiver", "1")},
					Processors: []component.ID{component.NewIDWithName("exampleprocessor", "mutate"), component.NewID("exampleprocessor")},
					Exporters:  []component.ID{component.NewID("exampleexporter"), component.NewIDWithName("exampleexporter", "1")},
				},
			},
			expectedPerExporter: 2,
		},
		{
			name: "pipelines_multi_no_proc.yaml",
			pipelineConfigs: map[component.ID]*PipelineConfig{
				component.NewID("traces"): {
					Receivers: []component.ID{component.NewID("examplereceiver"), component.NewIDWithName("examplereceiver", "1")},
					Exporters: []component.ID{component.NewID("exampleexporter"), component.NewIDWithName("exampleexporter", "1")},
				},
				component.NewID("metrics"): {
					Receivers: []component.ID{component.NewID("examplereceiver"), component.NewIDWithName("examplereceiver", "1")},
					Exporters: []component.ID{component.NewID("exampleexporter"), component.NewIDWithName("exampleexporter", "1")},
				},
				component.NewID("logs"): {
					Receivers: []component.ID{component.NewID("examplereceiver"), component.NewIDWithName("examplereceiver", "1")},
					Exporters: []component.ID{component.NewID("exampleexporter"), component.NewIDWithName("exampleexporter", "1")},
				},
			},
			expectedPerExporter: 2,
		},
		{
			name: "multi_pipeline_receivers_and_exporters.yaml",
			pipelineConfigs: map[component.ID]*PipelineConfig{
				component.NewID("traces"): {
					Receivers:  []component.ID{component.NewID("examplereceiver")},
					Processors: []component.ID{component.NewIDWithName("exampleprocessor", "mutate")},
					Exporters:  []component.ID{component.NewID("exampleexporter")},
				},
				component.NewIDWithName("traces", "1"): {
					Receivers: []component.ID{component.NewID("examplereceiver")},
					Exporters: []component.ID{component.NewID("exampleexporter")},
				},
				component.NewID("metrics"): {
					Receivers:  []component.ID{component.NewID("examplereceiver")},
					Processors: []component.ID{component.NewIDWithName("exampleprocessor", "mutate")},
					Exporters:  []component.ID{component.NewID("exampleexporter")},
				},
				component.NewIDWithName("metrics", "1"): {
					Receivers: []component.ID{component.NewID("examplereceiver")},
					Exporters: []component.ID{component.NewID("exampleexporter")},
				},
				component.NewID("logs"): {
					Receivers:  []component.ID{component.NewID("examplereceiver")},
					Processors: []component.ID{component.NewIDWithName("exampleprocessor", "mutate")},
					Exporters:  []component.ID{component.NewID("exampleexporter")},
				},
				component.NewIDWithName("logs", "1"): {
					Receivers: []component.ID{component.NewID("examplereceiver")},
					Exporters: []component.ID{component.NewID("exampleexporter")},
				},
			},
			expectedPerExporter: 2,
		},
		{
			name: "pipelines_conn_simple_traces.yaml",
			pipelineConfigs: map[component.ID]*PipelineConfig{
				component.NewIDWithName("traces", "in"): {
					Receivers:  []component.ID{component.NewID("examplereceiver")},
					Processors: []component.ID{component.NewID("exampleprocessor")},
					Exporters:  []component.ID{component.NewID("exampleconnector")},
				},
				component.NewIDWithName("traces", "out"): {
					Receivers:  []component.ID{component.NewID("exampleconnector")},
					Processors: []component.ID{component.NewIDWithName("exampleprocessor", "mutate")},
					Exporters:  []component.ID{component.NewID("exampleexporter")},
				},
			},
			expectedPerExporter: 1,
		},
		{
			name: "pipelines_conn_simple_metrics.yaml",
			pipelineConfigs: map[component.ID]*PipelineConfig{
				component.NewIDWithName("metrics", "in"): {
					Receivers:  []component.ID{component.NewID("examplereceiver")},
					Processors: []component.ID{component.NewID("exampleprocessor")},
					Exporters:  []component.ID{component.NewID("exampleconnector")},
				},
				component.NewIDWithName("metrics", "out"): {
					Receivers:  []component.ID{component.NewID("exampleconnector")},
					Processors: []component.ID{component.NewIDWithName("exampleprocessor", "mutate")},
					Exporters:  []component.ID{component.NewID("exampleexporter")},
				},
			},
			expectedPerExporter: 1,
		},
		{
			name: "pipelines_conn_simple_logs.yaml",
			pipelineConfigs: map[component.ID]*PipelineConfig{
				component.NewIDWithName("logs", "in"): {
					Receivers:  []component.ID{component.NewID("examplereceiver")},
					Processors: []component.ID{component.NewID("exampleprocessor")},
					Exporters:  []component.ID{component.NewID("exampleconnector")},
				},
				component.NewIDWithName("logs", "out"): {
					Receivers:  []component.ID{component.NewID("exampleconnector")},
					Processors: []component.ID{component.NewIDWithName("exampleprocessor", "mutate")},
					Exporters:  []component.ID{component.NewID("exampleexporter")},
				},
			},
			expectedPerExporter: 1,
		},
		{
			name: "pipelines_conn_fork_merge_traces.yaml",
			pipelineConfigs: map[component.ID]*PipelineConfig{
				component.NewIDWithName("traces", "in"): {
					Receivers:  []component.ID{component.NewID("examplereceiver")},
					Processors: []component.ID{component.NewID("exampleprocessor")},
					Exporters:  []component.ID{component.NewIDWithName("exampleconnector", "fork")},
				},
				component.NewIDWithName("traces", "type0"): {
					Receivers:  []component.ID{component.NewIDWithName("exampleconnector", "fork")},
					Processors: []component.ID{component.NewID("exampleprocessor")},
					Exporters:  []component.ID{component.NewIDWithName("exampleconnector", "merge")},
				},
				component.NewIDWithName("traces", "type1"): {
					Receivers:  []component.ID{component.NewIDWithName("exampleconnector", "fork")},
					Processors: []component.ID{component.NewIDWithName("exampleprocessor", "mutate")},
					Exporters:  []component.ID{component.NewIDWithName("exampleconnector", "merge")},
				},
				component.NewIDWithName("traces", "out"): {
					Receivers:  []component.ID{component.NewIDWithName("exampleconnector", "merge")},
					Processors: []component.ID{component.NewID("exampleprocessor")},
					Exporters:  []component.ID{component.NewID("exampleexporter")},
				},
			},
			expectedPerExporter: 2,
		},
		{
			name: "pipelines_conn_fork_merge_metrics.yaml",
			pipelineConfigs: map[component.ID]*PipelineConfig{
				component.NewIDWithName("metrics", "in"): {
					Receivers:  []component.ID{component.NewID("examplereceiver")},
					Processors: []component.ID{component.NewID("exampleprocessor")},
					Exporters:  []component.ID{component.NewIDWithName("exampleconnector", "fork")},
				},
				component.NewIDWithName("metrics", "type0"): {
					Receivers:  []component.ID{component.NewIDWithName("exampleconnector", "fork")},
					Processors: []component.ID{component.NewID("exampleprocessor")},
					Exporters:  []component.ID{component.NewIDWithName("exampleconnector", "merge")},
				},
				component.NewIDWithName("metrics", "type1"): {
					Receivers:  []component.ID{component.NewIDWithName("exampleconnector", "fork")},
					Processors: []component.ID{component.NewIDWithName("exampleprocessor", "mutate")},
					Exporters:  []component.ID{component.NewIDWithName("exampleconnector", "merge")},
				},
				component.NewIDWithName("metrics", "out"): {
					Receivers:  []component.ID{component.NewIDWithName("exampleconnector", "merge")},
					Processors: []component.ID{component.NewID("exampleprocessor")},
					Exporters:  []component.ID{component.NewID("exampleexporter")},
				},
			},
			expectedPerExporter: 2,
		},
		{
			name: "pipelines_conn_fork_merge_logs.yaml",
			pipelineConfigs: map[component.ID]*PipelineConfig{
				component.NewIDWithName("logs", "in"): {
					Receivers:  []component.ID{component.NewID("examplereceiver")},
					Processors: []component.ID{component.NewID("exampleprocessor")},
					Exporters:  []component.ID{component.NewIDWithName("exampleconnector", "fork")},
				},
				component.NewIDWithName("logs", "type0"): {
					Receivers:  []component.ID{component.NewIDWithName("exampleconnector", "fork")},
					Processors: []component.ID{component.NewID("exampleprocessor")},
					Exporters:  []component.ID{component.NewIDWithName("exampleconnector", "merge")},
				},
				component.NewIDWithName("logs", "type1"): {
					Receivers:  []component.ID{component.NewIDWithName("exampleconnector", "fork")},
					Processors: []component.ID{component.NewIDWithName("exampleprocessor", "mutate")},
					Exporters:  []component.ID{component.NewIDWithName("exampleconnector", "merge")},
				},
				component.NewIDWithName("logs", "out"): {
					Receivers:  []component.ID{component.NewIDWithName("exampleconnector", "merge")},
					Processors: []component.ID{component.NewID("exampleprocessor")},
					Exporters:  []component.ID{component.NewID("exampleexporter")},
				},
			},
			expectedPerExporter: 2,
		},
		{
			name: "pipelines_conn_translate_from_traces.yaml",
			pipelineConfigs: map[component.ID]*PipelineConfig{
				component.NewID("traces"): {
					Receivers:  []component.ID{component.NewID("examplereceiver")},
					Processors: []component.ID{component.NewID("exampleprocessor")},
					Exporters:  []component.ID{component.NewID("exampleconnector")},
				},
				component.NewID("metrics"): {
					Receivers:  []component.ID{component.NewID("exampleconnector")},
					Processors: []component.ID{component.NewID("exampleprocessor")},
					Exporters:  []component.ID{component.NewID("exampleexporter")},
				},
				component.NewID("logs"): {
					Receivers:  []component.ID{component.NewID("exampleconnector")},
					Processors: []component.ID{component.NewID("exampleprocessor")},
					Exporters:  []component.ID{component.NewID("exampleexporter")},
				},
			},
			expectedPerExporter: 1,
		},
		{
			name: "pipelines_conn_translate_from_metrics.yaml",
			pipelineConfigs: map[component.ID]*PipelineConfig{
				component.NewID("metrics"): {
					Receivers:  []component.ID{component.NewID("examplereceiver")},
					Processors: []component.ID{component.NewID("exampleprocessor")},
					Exporters:  []component.ID{component.NewID("exampleconnector")},
				},
				component.NewID("traces"): {
					Receivers:  []component.ID{component.NewID("exampleconnector")},
					Processors: []component.ID{component.NewID("exampleprocessor")},
					Exporters:  []component.ID{component.NewID("exampleexporter")},
				},
				component.NewID("logs"): {
					Receivers:  []component.ID{component.NewID("exampleconnector")},
					Processors: []component.ID{component.NewID("exampleprocessor")},
					Exporters:  []component.ID{component.NewID("exampleexporter")},
				},
			},
			expectedPerExporter: 1,
		},
		{
			name: "pipelines_conn_translate_from_logs.yaml",
			pipelineConfigs: map[component.ID]*PipelineConfig{
				component.NewID("logs"): {
					Receivers:  []component.ID{component.NewID("examplereceiver")},
					Processors: []component.ID{component.NewID("exampleprocessor")},
					Exporters:  []component.ID{component.NewID("exampleconnector")},
				},
				component.NewID("traces"): {
					Receivers:  []component.ID{component.NewID("exampleconnector")},
					Processors: []component.ID{component.NewID("exampleprocessor")},
					Exporters:  []component.ID{component.NewID("exampleexporter")},
				},
				component.NewID("metrics"): {
					Receivers:  []component.ID{component.NewID("exampleconnector")},
					Processors: []component.ID{component.NewID("exampleprocessor")},
					Exporters:  []component.ID{component.NewID("exampleexporter")},
				},
			},
			expectedPerExporter: 1,
		},
		{
			name: "pipelines_conn_matrix.yaml",
			pipelineConfigs: map[component.ID]*PipelineConfig{
				component.NewIDWithName("traces", "in"): {
					Receivers:  []component.ID{component.NewID("examplereceiver")},
					Processors: []component.ID{component.NewID("exampleprocessor")},
					Exporters:  []component.ID{component.NewID("exampleconnector")},
				},
				component.NewIDWithName("metrics", "in"): {
					Receivers:  []component.ID{component.NewID("examplereceiver")},
					Processors: []component.ID{component.NewIDWithName("exampleprocessor", "mutate")},
					Exporters:  []component.ID{component.NewID("exampleconnector")},
				},
				component.NewIDWithName("logs", "in"): {
					Receivers:  []component.ID{component.NewID("examplereceiver")},
					Processors: []component.ID{component.NewID("exampleprocessor")},
					Exporters:  []component.ID{component.NewID("exampleconnector")},
				},
				component.NewIDWithName("traces", "out"): {
					Receivers:  []component.ID{component.NewID("exampleconnector")},
					Processors: []component.ID{component.NewIDWithName("exampleprocessor", "mutate")},
					Exporters:  []component.ID{component.NewID("exampleexporter")},
				},
				component.NewIDWithName("metrics", "out"): {
					Receivers:  []component.ID{component.NewID("exampleconnector")},
					Processors: []component.ID{component.NewID("exampleprocessor")},
					Exporters:  []component.ID{component.NewID("exampleexporter")},
				},
				component.NewIDWithName("logs", "out"): {
					Receivers:  []component.ID{component.NewID("exampleconnector")},
					Processors: []component.ID{component.NewIDWithName("exampleprocessor", "mutate")},
					Exporters:  []component.ID{component.NewID("exampleexporter")},
				},
			},
			expectedPerExporter: 3,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Build the pipeline
			set := pipelinesSettings{
				Telemetry: componenttest.NewNopTelemetrySettings(),
				BuildInfo: component.NewDefaultBuildInfo(),
				ReceiverBuilder: receiver.NewBuilder(
					map[component.ID]component.Config{
						component.NewID("examplereceiver"):              testcomponents.ExampleReceiverFactory.CreateDefaultConfig(),
						component.NewIDWithName("examplereceiver", "1"): testcomponents.ExampleReceiverFactory.CreateDefaultConfig(),
					},
					map[component.Type]receiver.Factory{
						testcomponents.ExampleReceiverFactory.Type(): testcomponents.ExampleReceiverFactory,
					},
				),
				ProcessorBuilder: processor.NewBuilder(
					map[component.ID]component.Config{
						component.NewID("exampleprocessor"):                   testcomponents.ExampleProcessorFactory.CreateDefaultConfig(),
						component.NewIDWithName("exampleprocessor", "mutate"): testcomponents.ExampleProcessorFactory.CreateDefaultConfig(),
					},
					map[component.Type]processor.Factory{
						testcomponents.ExampleProcessorFactory.Type(): testcomponents.ExampleProcessorFactory,
					},
				),
				ExporterBuilder: exporter.NewBuilder(
					map[component.ID]component.Config{
						component.NewID("exampleexporter"):              testcomponents.ExampleExporterFactory.CreateDefaultConfig(),
						component.NewIDWithName("exampleexporter", "1"): testcomponents.ExampleExporterFactory.CreateDefaultConfig(),
					},
					map[component.Type]exporter.Factory{
						testcomponents.ExampleExporterFactory.Type(): testcomponents.ExampleExporterFactory,
					},
				),
				ConnectorBuilder: connector.NewBuilder(
					map[component.ID]component.Config{
						component.NewID("exampleconnector"):                  testcomponents.ExampleConnectorFactory.CreateDefaultConfig(),
						component.NewIDWithName("exampleconnector", "fork"):  testcomponents.ExampleConnectorFactory.CreateDefaultConfig(),
						component.NewIDWithName("exampleconnector", "merge"): testcomponents.ExampleConnectorFactory.CreateDefaultConfig(),
					},
					map[component.Type]connector.Factory{
						testcomponents.ExampleConnectorFactory.Type(): testcomponents.ExampleConnectorFactory,
					},
				),
				PipelineConfigs: test.pipelineConfigs,
			}

			pipelinesInterface, err := buildPipelinesGraph(context.Background(), set)
			require.NoError(t, err)

			pg, ok := pipelinesInterface.(*pipelinesGraph)
			require.True(t, ok)

			assert.Equal(t, len(test.pipelineConfigs), len(pg.pipelines))

			assert.NoError(t, pg.StartAll(context.Background(), componenttest.NewNopHost()))

			// Check each pipeline individually, ensuring that all components are started
			// and that they have observed no signals yet.
			for pipelineID, pipelineCfg := range test.pipelineConfigs {
				pipeline, ok := pg.pipelines[pipelineID]
				require.True(t, ok, "expected to find pipeline: %s", pipelineID.String())

				// Determine independently if the capabilities node should report MutateData as true
				var expectMutatesData bool
				for _, proc := range pipelineCfg.Processors {
					if proc.Name() == "mutate" {
						expectMutatesData = true
					}
				}
				assert.Equal(t, expectMutatesData, pipeline.capabilitiesNode.getConsumer().Capabilities().MutatesData)

				expectedReceivers, expectedExporters := expectedInstances(test.pipelineConfigs, pipelineID)
				require.Equal(t, expectedReceivers, len(pipeline.receivers))
				require.Equal(t, len(pipelineCfg.Processors), len(pipeline.processors))
				require.Equal(t, expectedExporters, len(pipeline.exporters))

				for _, n := range pipeline.exporters {
					switch c := n.(type) {
					case *exporterNode:
						e := c.Component.(*testcomponents.ExampleExporter)
						require.True(t, e.Started())
						require.Equal(t, 0, len(e.Traces))
						require.Equal(t, 0, len(e.Metrics))
						require.Equal(t, 0, len(e.Logs))
					case *connectorNode:
						// connector needs to be unwrapped to access component as ExampleConnector
						switch ct := c.Component.(type) {
						case connector.Traces:
							require.True(t, ct.(*testcomponents.ExampleConnector).Started())
						case connector.Metrics:
							require.True(t, ct.(*testcomponents.ExampleConnector).Started())
						case connector.Logs:
							require.True(t, ct.(*testcomponents.ExampleConnector).Started())
						}
					default:
						require.Fail(t, fmt.Sprintf("unexpected type %T", c))
					}
				}

				for _, n := range pipeline.processors {
					require.True(t, n.Component.(*testcomponents.ExampleProcessor).Started())
				}

				for _, n := range pipeline.receivers {
					switch c := n.(type) {
					case *receiverNode:
						require.True(t, c.Component.(*testcomponents.ExampleReceiver).Started())
					case *connectorNode:
						// connector needs to be unwrapped to access component as ExampleConnector
						switch ct := c.Component.(type) {
						case connector.Traces:
							require.True(t, ct.(*testcomponents.ExampleConnector).Started())
						case connector.Metrics:
							require.True(t, ct.(*testcomponents.ExampleConnector).Started())
						case connector.Logs:
							require.True(t, ct.(*testcomponents.ExampleConnector).Started())
						}
					default:
						require.Fail(t, fmt.Sprintf("unexpected type %T", c))
					}
				}
			}

			// Push data into the pipelines. The list of receivers is retrieved directly from the overall
			// component graph because we do not want to duplicate signal inputs to receivers that are
			// shared between pipelines. The `allReceivers` function also excludes connectors, which we do
			// not want to directly inject with signals.
			allReceivers := pg.getReceivers()
			for _, c := range allReceivers[component.DataTypeTraces] {
				tracesReceiver := c.(*testcomponents.ExampleReceiver)
				assert.NoError(t, tracesReceiver.ConsumeTraces(context.Background(), testdata.GenerateTraces(1)))
			}
			for _, c := range allReceivers[component.DataTypeMetrics] {
				metricsReceiver := c.(*testcomponents.ExampleReceiver)
				assert.NoError(t, metricsReceiver.ConsumeMetrics(context.Background(), testdata.GenerateMetrics(1)))
			}
			for _, c := range allReceivers[component.DataTypeLogs] {
				logsReceiver := c.(*testcomponents.ExampleReceiver)
				assert.NoError(t, logsReceiver.ConsumeLogs(context.Background(), testdata.GenerateLogs(1)))
			}

			// Shut down the entire component graph
			assert.NoError(t, pg.ShutdownAll(context.Background()))

			// Check each pipeline individually, ensuring that all components are stopped.
			for pipelineID := range test.pipelineConfigs {
				pipeline, ok := pg.pipelines[pipelineID]
				require.True(t, ok, "expected to find pipeline: %s", pipelineID.String())

				for _, n := range pipeline.receivers {
					switch c := n.(type) {
					case *receiverNode:
						require.True(t, c.Component.(*testcomponents.ExampleReceiver).Stopped())
					case *connectorNode:
						// connector needs to be unwrapped to access component as ExampleConnector
						switch ct := c.Component.(type) {
						case connector.Traces:
							require.True(t, ct.(*testcomponents.ExampleConnector).Stopped())
						case connector.Metrics:
							require.True(t, ct.(*testcomponents.ExampleConnector).Stopped())
						case connector.Logs:
							require.True(t, ct.(*testcomponents.ExampleConnector).Stopped())
						}
					default:
						require.Fail(t, fmt.Sprintf("unexpected type %T", c))
					}
				}

				for _, n := range pipeline.processors {
					require.True(t, n.Component.(*testcomponents.ExampleProcessor).Stopped())
				}

				for _, n := range pipeline.exporters {
					switch c := n.(type) {
					case *exporterNode:
						e := c.Component.(*testcomponents.ExampleExporter)
						require.True(t, e.Stopped())
					case *connectorNode:
						// connector needs to be unwrapped to access component as ExampleConnector
						switch ct := c.Component.(type) {
						case connector.Traces:
							require.True(t, ct.(*testcomponents.ExampleConnector).Stopped())
						case connector.Metrics:
							require.True(t, ct.(*testcomponents.ExampleConnector).Stopped())
						case connector.Logs:
							require.True(t, ct.(*testcomponents.ExampleConnector).Stopped())
						}
					default:
						require.Fail(t, fmt.Sprintf("unexpected type %T", c))
					}
				}
			}

			// Get the list of exporters directly from the overall component graph. Like receivers,
			// exclude connectors and validate each exporter once regardless of sharing between pipelines.
			allExporters := pg.GetExporters()
			for _, e := range allExporters[component.DataTypeTraces] {
				tracesExporter := e.(*testcomponents.ExampleExporter)
				assert.Equal(t, test.expectedPerExporter, len(tracesExporter.Traces))
				for i := 0; i < test.expectedPerExporter; i++ {
					assert.EqualValues(t, testdata.GenerateTraces(1), tracesExporter.Traces[0])
				}
			}
			for _, e := range allExporters[component.DataTypeMetrics] {
				metricsExporter := e.(*testcomponents.ExampleExporter)
				assert.Equal(t, test.expectedPerExporter, len(metricsExporter.Metrics))
				for i := 0; i < test.expectedPerExporter; i++ {
					assert.EqualValues(t, testdata.GenerateMetrics(1), metricsExporter.Metrics[0])
				}
			}
			for _, e := range allExporters[component.DataTypeLogs] {
				logsExporter := e.(*testcomponents.ExampleExporter)
				assert.Equal(t, test.expectedPerExporter, len(logsExporter.Logs))
				for i := 0; i < test.expectedPerExporter; i++ {
					assert.EqualValues(t, testdata.GenerateLogs(1), logsExporter.Logs[0])
				}
			}
		})
	}
}

func TestGraphBuildErrors(t *testing.T) {
	nopReceiverFactory := receivertest.NewNopFactory()
	nopProcessorFactory := processortest.NewNopFactory()
	nopExporterFactory := exportertest.NewNopFactory()
	nopConnectorFactory := connectortest.NewNopFactory()
	badReceiverFactory := newBadReceiverFactory()
	badProcessorFactory := newBadProcessorFactory()
	badExporterFactory := newBadExporterFactory()
	badConnectorFactory := newBadConnectorFactory()

	tests := []struct {
		name               string
		receiverCfgs       map[component.ID]component.Config
		processorCfgs      map[component.ID]component.Config
		exporterCfgs       map[component.ID]component.Config
		connectorCfgs      map[component.ID]component.Config
		pipelineCfgs       map[component.ID]*PipelineConfig
		expected           string
		expectedStartsWith string
	}{
		{
			name: "not_supported_exporter_logs",
			receiverCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopReceiverFactory.CreateDefaultConfig(),
			},
			exporterCfgs: map[component.ID]component.Config{
				component.NewID("bf"): badExporterFactory.CreateDefaultConfig(),
			},
			pipelineCfgs: map[component.ID]*PipelineConfig{
				component.NewID("logs"): {
					Receivers: []component.ID{component.NewID("nop")},
					Exporters: []component.ID{component.NewID("bf")},
				},
			},
			expected: "failed to create \"bf\" exporter, in pipeline \"logs/*\": telemetry type is not supported",
		},
		{
			name: "not_supported_exporter_metrics",
			receiverCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopReceiverFactory.CreateDefaultConfig(),
			},
			exporterCfgs: map[component.ID]component.Config{
				component.NewID("bf"): badExporterFactory.CreateDefaultConfig(),
			},
			pipelineCfgs: map[component.ID]*PipelineConfig{
				component.NewID("metrics"): {
					Receivers: []component.ID{component.NewID("nop")},
					Exporters: []component.ID{component.NewID("bf")},
				},
			},
			expected: "failed to create \"bf\" exporter, in pipeline \"metrics/*\": telemetry type is not supported",
		},
		{
			name: "not_supported_exporter_traces",
			receiverCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopReceiverFactory.CreateDefaultConfig(),
			},
			exporterCfgs: map[component.ID]component.Config{
				component.NewID("bf"): badExporterFactory.CreateDefaultConfig(),
			},
			pipelineCfgs: map[component.ID]*PipelineConfig{
				component.NewID("traces"): {
					Receivers: []component.ID{component.NewID("nop")},
					Exporters: []component.ID{component.NewID("bf")},
				},
			},
			expected: "failed to create \"bf\" exporter, in pipeline \"traces/*\": telemetry type is not supported",
		},
		{
			name: "not_supported_processor_logs",
			receiverCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopReceiverFactory.CreateDefaultConfig(),
			},
			processorCfgs: map[component.ID]component.Config{
				component.NewID("bf"): badProcessorFactory.CreateDefaultConfig(),
			},
			exporterCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopReceiverFactory.CreateDefaultConfig(),
			},
			pipelineCfgs: map[component.ID]*PipelineConfig{
				component.NewID("logs"): {
					Receivers:  []component.ID{component.NewID("nop")},
					Processors: []component.ID{component.NewID("bf")},
					Exporters:  []component.ID{component.NewID("nop")},
				},
			},
			expected: "failed to create \"bf\" processor, in pipeline \"logs\": telemetry type is not supported",
		},
		{
			name: "not_supported_processor_metrics",
			receiverCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopReceiverFactory.CreateDefaultConfig(),
			},
			processorCfgs: map[component.ID]component.Config{
				component.NewID("bf"): badProcessorFactory.CreateDefaultConfig(),
			},
			exporterCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopReceiverFactory.CreateDefaultConfig(),
			},
			pipelineCfgs: map[component.ID]*PipelineConfig{
				component.NewID("metrics"): {
					Receivers:  []component.ID{component.NewID("nop")},
					Processors: []component.ID{component.NewID("bf")},
					Exporters:  []component.ID{component.NewID("nop")},
				},
			},
			expected: "failed to create \"bf\" processor, in pipeline \"metrics\": telemetry type is not supported",
		},
		{
			name: "not_supported_processor_traces",
			receiverCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopReceiverFactory.CreateDefaultConfig(),
			},
			processorCfgs: map[component.ID]component.Config{
				component.NewID("bf"): badProcessorFactory.CreateDefaultConfig(),
			},
			exporterCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopReceiverFactory.CreateDefaultConfig(),
			},
			pipelineCfgs: map[component.ID]*PipelineConfig{
				component.NewID("traces"): {
					Receivers:  []component.ID{component.NewID("nop")},
					Processors: []component.ID{component.NewID("bf")},
					Exporters:  []component.ID{component.NewID("nop")},
				},
			},
			expected: "failed to create \"bf\" processor, in pipeline \"traces\": telemetry type is not supported",
		},
		{
			name: "not_supported_receiver_logs",
			receiverCfgs: map[component.ID]component.Config{
				component.NewID("bf"): badReceiverFactory.CreateDefaultConfig(),
			},
			exporterCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopReceiverFactory.CreateDefaultConfig(),
			},
			pipelineCfgs: map[component.ID]*PipelineConfig{
				component.NewID("logs"): {
					Receivers: []component.ID{component.NewID("bf")},
					Exporters: []component.ID{component.NewID("nop")},
				},
			},
			expected: "failed to create \"bf\" receiver, in pipeline \"logs/*\": telemetry type is not supported",
		},
		{
			name: "not_supported_receiver_metrics",
			receiverCfgs: map[component.ID]component.Config{
				component.NewID("bf"): badReceiverFactory.CreateDefaultConfig(),
			},
			exporterCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopReceiverFactory.CreateDefaultConfig(),
			},
			pipelineCfgs: map[component.ID]*PipelineConfig{
				component.NewID("metrics"): {
					Receivers: []component.ID{component.NewID("bf")},
					Exporters: []component.ID{component.NewID("nop")},
				},
			},
			expected: "failed to create \"bf\" receiver, in pipeline \"metrics/*\": telemetry type is not supported",
		},
		{
			name: "not_supported_receiver_traces",
			receiverCfgs: map[component.ID]component.Config{
				component.NewID("bf"): badReceiverFactory.CreateDefaultConfig(),
			},
			exporterCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopReceiverFactory.CreateDefaultConfig(),
			},
			pipelineCfgs: map[component.ID]*PipelineConfig{
				component.NewID("traces"): {
					Receivers: []component.ID{component.NewID("bf")},
					Exporters: []component.ID{component.NewID("nop")},
				},
			},
			expected: "failed to create \"bf\" receiver, in pipeline \"traces/*\": telemetry type is not supported",
		},
		{
			name: "not_supported_connector_traces_traces.yaml",
			receiverCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopReceiverFactory.CreateDefaultConfig(),
			},
			exporterCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopExporterFactory.CreateDefaultConfig(),
			},
			connectorCfgs: map[component.ID]component.Config{
				component.NewID("bf"): nopConnectorFactory.CreateDefaultConfig(),
			},
			pipelineCfgs: map[component.ID]*PipelineConfig{
				component.NewIDWithName("traces", "in"): {
					Receivers: []component.ID{component.NewID("nop")},
					Exporters: []component.ID{component.NewID("bf")},
				},
				component.NewIDWithName("traces", "out"): {
					Receivers: []component.ID{component.NewID("bf")},
					Exporters: []component.ID{component.NewID("nop")},
				},
			},
			expected: "connector \"bf\" cannot connect from traces to traces: telemetry type is not supported",
		},
		{
			name: "not_supported_connector_traces_metrics.yaml",
			receiverCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopReceiverFactory.CreateDefaultConfig(),
			},
			exporterCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopExporterFactory.CreateDefaultConfig(),
			},
			connectorCfgs: map[component.ID]component.Config{
				component.NewID("bf"): nopConnectorFactory.CreateDefaultConfig(),
			},
			pipelineCfgs: map[component.ID]*PipelineConfig{
				component.NewIDWithName("traces", "in"): {
					Receivers: []component.ID{component.NewID("nop")},
					Exporters: []component.ID{component.NewID("bf")},
				},
				component.NewIDWithName("metrics", "out"): {
					Receivers: []component.ID{component.NewID("bf")},
					Exporters: []component.ID{component.NewID("nop")},
				},
			},
			expected: "connector \"bf\" cannot connect from traces to metrics: telemetry type is not supported",
		},
		{
			name: "not_supported_connector_traces_logs.yaml",
			receiverCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopReceiverFactory.CreateDefaultConfig(),
			},
			exporterCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopExporterFactory.CreateDefaultConfig(),
			},
			connectorCfgs: map[component.ID]component.Config{
				component.NewID("bf"): nopConnectorFactory.CreateDefaultConfig(),
			},
			pipelineCfgs: map[component.ID]*PipelineConfig{
				component.NewIDWithName("traces", "in"): {
					Receivers: []component.ID{component.NewID("nop")},
					Exporters: []component.ID{component.NewID("bf")},
				},
				component.NewIDWithName("logs", "out"): {
					Receivers: []component.ID{component.NewID("bf")},
					Exporters: []component.ID{component.NewID("nop")},
				},
			},
			expected: "connector \"bf\" cannot connect from traces to logs: telemetry type is not supported",
		},
		{
			name: "not_supported_connector_metrics_traces.yaml",
			receiverCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopReceiverFactory.CreateDefaultConfig(),
			},
			exporterCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopExporterFactory.CreateDefaultConfig(),
			},
			connectorCfgs: map[component.ID]component.Config{
				component.NewID("bf"): nopConnectorFactory.CreateDefaultConfig(),
			},
			pipelineCfgs: map[component.ID]*PipelineConfig{
				component.NewIDWithName("metrics", "in"): {
					Receivers: []component.ID{component.NewID("nop")},
					Exporters: []component.ID{component.NewID("bf")},
				},
				component.NewIDWithName("traces", "out"): {
					Receivers: []component.ID{component.NewID("bf")},
					Exporters: []component.ID{component.NewID("nop")},
				},
			},
			expected: "connector \"bf\" cannot connect from metrics to traces: telemetry type is not supported",
		},
		{
			name: "not_supported_connector_metrics_metrics.yaml",
			receiverCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopReceiverFactory.CreateDefaultConfig(),
			},
			exporterCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopExporterFactory.CreateDefaultConfig(),
			},
			connectorCfgs: map[component.ID]component.Config{
				component.NewID("bf"): nopConnectorFactory.CreateDefaultConfig(),
			},
			pipelineCfgs: map[component.ID]*PipelineConfig{
				component.NewIDWithName("metrics", "in"): {
					Receivers: []component.ID{component.NewID("nop")},
					Exporters: []component.ID{component.NewID("bf")},
				},
				component.NewIDWithName("metrics", "out"): {
					Receivers: []component.ID{component.NewID("bf")},
					Exporters: []component.ID{component.NewID("nop")},
				},
			},
			expected: "connector \"bf\" cannot connect from metrics to metrics: telemetry type is not supported",
		},
		{
			name: "not_supported_connector_metrics_logs.yaml",
			receiverCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopReceiverFactory.CreateDefaultConfig(),
			},
			exporterCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopExporterFactory.CreateDefaultConfig(),
			},
			connectorCfgs: map[component.ID]component.Config{
				component.NewID("bf"): nopConnectorFactory.CreateDefaultConfig(),
			},
			pipelineCfgs: map[component.ID]*PipelineConfig{
				component.NewIDWithName("metrics", "in"): {
					Receivers: []component.ID{component.NewID("nop")},
					Exporters: []component.ID{component.NewID("bf")},
				},
				component.NewIDWithName("logs", "out"): {
					Receivers: []component.ID{component.NewID("bf")},
					Exporters: []component.ID{component.NewID("nop")},
				},
			},
			expected: "connector \"bf\" cannot connect from metrics to logs: telemetry type is not supported",
		},
		{
			name: "not_supported_connector_logs_traces.yaml",
			receiverCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopReceiverFactory.CreateDefaultConfig(),
			},
			exporterCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopExporterFactory.CreateDefaultConfig(),
			},
			connectorCfgs: map[component.ID]component.Config{
				component.NewID("bf"): nopConnectorFactory.CreateDefaultConfig(),
			},
			pipelineCfgs: map[component.ID]*PipelineConfig{
				component.NewIDWithName("logs", "in"): {
					Receivers: []component.ID{component.NewID("nop")},
					Exporters: []component.ID{component.NewID("bf")},
				},
				component.NewIDWithName("traces", "out"): {
					Receivers: []component.ID{component.NewID("bf")},
					Exporters: []component.ID{component.NewID("nop")},
				},
			},
			expected: "connector \"bf\" cannot connect from logs to traces: telemetry type is not supported",
		},
		{
			name: "not_supported_connector_logs_metrics.yaml",
			receiverCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopReceiverFactory.CreateDefaultConfig(),
			},
			exporterCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopExporterFactory.CreateDefaultConfig(),
			},
			connectorCfgs: map[component.ID]component.Config{
				component.NewID("bf"): nopConnectorFactory.CreateDefaultConfig(),
			},
			pipelineCfgs: map[component.ID]*PipelineConfig{
				component.NewIDWithName("logs", "in"): {
					Receivers: []component.ID{component.NewID("nop")},
					Exporters: []component.ID{component.NewID("bf")},
				},
				component.NewIDWithName("metrics", "out"): {
					Receivers: []component.ID{component.NewID("bf")},
					Exporters: []component.ID{component.NewID("nop")},
				},
			},
			expected: "connector \"bf\" cannot connect from logs to metrics: telemetry type is not supported",
		},
		{
			name: "not_supported_connector_logs_logs.yaml",
			receiverCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopReceiverFactory.CreateDefaultConfig(),
			},
			exporterCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopExporterFactory.CreateDefaultConfig(),
			},
			connectorCfgs: map[component.ID]component.Config{
				component.NewID("bf"): nopConnectorFactory.CreateDefaultConfig(),
			},
			pipelineCfgs: map[component.ID]*PipelineConfig{
				component.NewIDWithName("logs", "in"): {
					Receivers: []component.ID{component.NewID("nop")},
					Exporters: []component.ID{component.NewID("bf")},
				},
				component.NewIDWithName("logs", "out"): {
					Receivers: []component.ID{component.NewID("bf")},
					Exporters: []component.ID{component.NewID("nop")},
				},
			},
			expected: "connector \"bf\" cannot connect from logs to logs: telemetry type is not supported",
		},
		{
			name: "not_allowed_simple_cycle_traces.yaml",
			receiverCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopReceiverFactory.CreateDefaultConfig(),
			},
			processorCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopProcessorFactory.CreateDefaultConfig(),
			},
			exporterCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopExporterFactory.CreateDefaultConfig(),
			},
			connectorCfgs: map[component.ID]component.Config{
				component.NewIDWithName("nop", "conn"): nopConnectorFactory.CreateDefaultConfig(),
			},
			pipelineCfgs: map[component.ID]*PipelineConfig{
				component.NewID("traces"): {
					Receivers:  []component.ID{component.NewIDWithName("nop", "conn")},
					Processors: []component.ID{component.NewID("nop")},
					Exporters:  []component.ID{component.NewIDWithName("nop", "conn")},
				},
			},
			expectedStartsWith: "topo: no topological ordering: cyclic components:",
		},
		{
			name: "not_allowed_simple_cycle_metrics.yaml",
			receiverCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopReceiverFactory.CreateDefaultConfig(),
			},
			processorCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopProcessorFactory.CreateDefaultConfig(),
			},
			exporterCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopExporterFactory.CreateDefaultConfig(),
			},
			connectorCfgs: map[component.ID]component.Config{
				component.NewIDWithName("nop", "conn"): nopConnectorFactory.CreateDefaultConfig(),
			},
			pipelineCfgs: map[component.ID]*PipelineConfig{
				component.NewID("metrics"): {
					Receivers:  []component.ID{component.NewIDWithName("nop", "conn")},
					Processors: []component.ID{component.NewID("nop")},
					Exporters:  []component.ID{component.NewIDWithName("nop", "conn")},
				},
			},
			expectedStartsWith: "topo: no topological ordering: cyclic components:",
		},
		{
			name: "not_allowed_simple_cycle_logs.yaml",
			receiverCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopReceiverFactory.CreateDefaultConfig(),
			},
			processorCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopProcessorFactory.CreateDefaultConfig(),
			},
			exporterCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopExporterFactory.CreateDefaultConfig(),
			},
			connectorCfgs: map[component.ID]component.Config{
				component.NewIDWithName("nop", "conn"): nopConnectorFactory.CreateDefaultConfig(),
			},
			pipelineCfgs: map[component.ID]*PipelineConfig{
				component.NewID("logs"): {
					Receivers:  []component.ID{component.NewIDWithName("nop", "conn")},
					Processors: []component.ID{component.NewID("nop")},
					Exporters:  []component.ID{component.NewIDWithName("nop", "conn")},
				},
			},
			expectedStartsWith: "topo: no topological ordering: cyclic components:",
		},
		{
			name: "not_allowed_deep_cycle_traces.yaml",
			receiverCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopReceiverFactory.CreateDefaultConfig(),
			},
			processorCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopProcessorFactory.CreateDefaultConfig(),
			},
			exporterCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopExporterFactory.CreateDefaultConfig(),
			},
			connectorCfgs: map[component.ID]component.Config{
				component.NewIDWithName("nop", "conn"):  nopConnectorFactory.CreateDefaultConfig(),
				component.NewIDWithName("nop", "conn1"): nopConnectorFactory.CreateDefaultConfig(),
				component.NewIDWithName("nop", "conn2"): nopConnectorFactory.CreateDefaultConfig(),
			},
			pipelineCfgs: map[component.ID]*PipelineConfig{
				component.NewIDWithName("traces", "in"): {
					Receivers:  []component.ID{component.NewID("nop")},
					Processors: []component.ID{component.NewID("nop")},
					Exporters:  []component.ID{component.NewIDWithName("nop", "conn")},
				},
				component.NewIDWithName("traces", "1"): {
					Receivers:  []component.ID{component.NewIDWithName("nop", "conn")},
					Processors: []component.ID{component.NewID("nop")},
					Exporters:  []component.ID{component.NewIDWithName("nop", "conn1")},
				},
				component.NewIDWithName("traces", "2"): {
					Receivers:  []component.ID{component.NewIDWithName("nop", "conn1")},
					Processors: []component.ID{component.NewID("nop")},
					Exporters:  []component.ID{component.NewIDWithName("nop", "conn2"), component.NewIDWithName("nop", "conn")},
				},
				component.NewIDWithName("traces", "out"): {
					Receivers:  []component.ID{component.NewIDWithName("nop", "conn2")},
					Processors: []component.ID{component.NewID("nop")},
					Exporters:  []component.ID{component.NewID("nop")},
				},
			},
			expectedStartsWith: "topo: no topological ordering: cyclic components:",
			// TODO rebuild cycle in order
		},
		{
			name: "not_allowed_deep_cycle_metrics.yaml",
			receiverCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopReceiverFactory.CreateDefaultConfig(),
			},
			processorCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopProcessorFactory.CreateDefaultConfig(),
			},
			exporterCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopExporterFactory.CreateDefaultConfig(),
			},
			connectorCfgs: map[component.ID]component.Config{
				component.NewIDWithName("nop", "conn"):  nopConnectorFactory.CreateDefaultConfig(),
				component.NewIDWithName("nop", "conn1"): nopConnectorFactory.CreateDefaultConfig(),
				component.NewIDWithName("nop", "conn2"): nopConnectorFactory.CreateDefaultConfig(),
			},
			pipelineCfgs: map[component.ID]*PipelineConfig{
				component.NewIDWithName("metrics", "in"): {
					Receivers:  []component.ID{component.NewID("nop")},
					Processors: []component.ID{component.NewID("nop")},
					Exporters:  []component.ID{component.NewIDWithName("nop", "conn")},
				},
				component.NewIDWithName("metrics", "1"): {
					Receivers:  []component.ID{component.NewIDWithName("nop", "conn")},
					Processors: []component.ID{component.NewID("nop")},
					Exporters:  []component.ID{component.NewIDWithName("nop", "conn1")},
				},
				component.NewIDWithName("metrics", "2"): {
					Receivers:  []component.ID{component.NewIDWithName("nop", "conn1")},
					Processors: []component.ID{component.NewID("nop")},
					Exporters:  []component.ID{component.NewIDWithName("nop", "conn2"), component.NewIDWithName("nop", "conn")},
				},
				component.NewIDWithName("metrics", "out"): {
					Receivers:  []component.ID{component.NewIDWithName("nop", "conn2")},
					Processors: []component.ID{component.NewID("nop")},
					Exporters:  []component.ID{component.NewID("nop")},
				},
			},
			expectedStartsWith: "topo: no topological ordering: cyclic components:",
			// TODO rebuild cycle in order
		},
		{
			name: "not_allowed_deep_cycle_logs.yaml",
			receiverCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopReceiverFactory.CreateDefaultConfig(),
			},
			processorCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopProcessorFactory.CreateDefaultConfig(),
			},
			exporterCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopExporterFactory.CreateDefaultConfig(),
			},
			connectorCfgs: map[component.ID]component.Config{
				component.NewIDWithName("nop", "conn"):  nopConnectorFactory.CreateDefaultConfig(),
				component.NewIDWithName("nop", "conn1"): nopConnectorFactory.CreateDefaultConfig(),
				component.NewIDWithName("nop", "conn2"): nopConnectorFactory.CreateDefaultConfig(),
			},
			pipelineCfgs: map[component.ID]*PipelineConfig{
				component.NewIDWithName("logs", "in"): {
					Receivers:  []component.ID{component.NewID("nop")},
					Processors: []component.ID{component.NewID("nop")},
					Exporters:  []component.ID{component.NewIDWithName("nop", "conn")},
				},
				component.NewIDWithName("logs", "1"): {
					Receivers:  []component.ID{component.NewIDWithName("nop", "conn")},
					Processors: []component.ID{component.NewID("nop")},
					Exporters:  []component.ID{component.NewIDWithName("nop", "conn1")},
				},
				component.NewIDWithName("logs", "2"): {
					Receivers:  []component.ID{component.NewIDWithName("nop", "conn1")},
					Processors: []component.ID{component.NewID("nop")},
					Exporters:  []component.ID{component.NewIDWithName("nop", "conn2"), component.NewIDWithName("nop", "conn")},
				},
				component.NewIDWithName("logs", "out"): {
					Receivers:  []component.ID{component.NewIDWithName("nop", "conn2")},
					Processors: []component.ID{component.NewID("nop")},
					Exporters:  []component.ID{component.NewID("nop")},
				},
			},
			expectedStartsWith: "topo: no topological ordering: cyclic components:",
			// TODO rebuild cycle in order
		},
		{
			name: "not_allowed_deep_cycle_multi_signal.yaml",
			receiverCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopReceiverFactory.CreateDefaultConfig(),
			},
			processorCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopProcessorFactory.CreateDefaultConfig(),
			},
			exporterCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopExporterFactory.CreateDefaultConfig(),
			},
			connectorCfgs: map[component.ID]component.Config{
				component.NewIDWithName("nop", "fork"):      nopConnectorFactory.CreateDefaultConfig(),
				component.NewIDWithName("nop", "count"):     nopConnectorFactory.CreateDefaultConfig(),
				component.NewIDWithName("nop", "forkagain"): nopConnectorFactory.CreateDefaultConfig(),
				component.NewIDWithName("nop", "rawlog"):    nopConnectorFactory.CreateDefaultConfig(),
			},
			pipelineCfgs: map[component.ID]*PipelineConfig{
				component.NewIDWithName("traces", "in"): {
					Receivers:  []component.ID{component.NewID("nop")},
					Processors: []component.ID{component.NewID("nop")},
					Exporters:  []component.ID{component.NewIDWithName("nop", "fork")},
				},
				component.NewIDWithName("traces", "copy1"): {
					Receivers:  []component.ID{component.NewIDWithName("nop", "fork")},
					Processors: []component.ID{component.NewID("nop")},
					Exporters:  []component.ID{component.NewIDWithName("nop", "count")},
				},
				component.NewIDWithName("traces", "copy2"): {
					Receivers:  []component.ID{component.NewIDWithName("nop", "fork")},
					Processors: []component.ID{component.NewID("nop")},
					Exporters:  []component.ID{component.NewIDWithName("nop", "forkagain")},
				},
				component.NewIDWithName("traces", "copy2a"): {
					Receivers:  []component.ID{component.NewIDWithName("nop", "forkagain")},
					Processors: []component.ID{component.NewID("nop")},
					Exporters:  []component.ID{component.NewIDWithName("nop", "count")},
				},
				component.NewIDWithName("traces", "copy2b"): {
					Receivers:  []component.ID{component.NewIDWithName("nop", "forkagain")},
					Processors: []component.ID{component.NewID("nop")},
					Exporters:  []component.ID{component.NewIDWithName("nop", "rawlog")},
				},
				component.NewIDWithName("metrics", "count"): {
					Receivers:  []component.ID{component.NewIDWithName("nop", "count")},
					Processors: []component.ID{component.NewID("nop")},
					Exporters:  []component.ID{component.NewID("nop")},
				},
				component.NewIDWithName("logs", "raw"): {
					Receivers:  []component.ID{component.NewIDWithName("nop", "rawlog")},
					Processors: []component.ID{component.NewID("nop")},
					Exporters:  []component.ID{component.NewIDWithName("nop", "fork")}, // cannot loop back to "nop/fork"
				},
			},
			expected: "topo: no topological ordering: 12 nodes in 1 cyclic components",
		},
		{
			name: "unknown_exporter_config",
			receiverCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopReceiverFactory.CreateDefaultConfig(),
			},
			exporterCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopReceiverFactory.CreateDefaultConfig(),
			},
			pipelineCfgs: map[component.ID]*PipelineConfig{
				component.NewID("traces"): {
					Receivers: []component.ID{component.NewID("nop")},
					Exporters: []component.ID{component.NewID("nop"), component.NewIDWithName("nop", "1")},
				},
			},
			expected: "failed to create \"nop/1\" exporter, in pipeline \"traces/*\": exporter \"nop/1\" is not configured",
		},
		{
			name: "unknown_exporter_factory",
			receiverCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopReceiverFactory.CreateDefaultConfig(),
			},
			exporterCfgs: map[component.ID]component.Config{
				component.NewID("unknown"): nopReceiverFactory.CreateDefaultConfig(),
			},
			pipelineCfgs: map[component.ID]*PipelineConfig{
				component.NewID("traces"): {
					Receivers: []component.ID{component.NewID("nop")},
					Exporters: []component.ID{component.NewID("unknown")},
				},
			},
			expected: "failed to create \"unknown\" exporter, in pipeline \"traces/*\": exporter factory not available for: \"unknown\"",
		},
		{
			name: "unknown_processor_config",
			receiverCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopReceiverFactory.CreateDefaultConfig(),
			},
			processorCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopProcessorFactory.CreateDefaultConfig(),
			},
			exporterCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopReceiverFactory.CreateDefaultConfig(),
			},
			pipelineCfgs: map[component.ID]*PipelineConfig{
				component.NewID("metrics"): {
					Receivers:  []component.ID{component.NewID("nop")},
					Processors: []component.ID{component.NewID("nop"), component.NewIDWithName("nop", "1")},
					Exporters:  []component.ID{component.NewID("nop")},
				},
			},
			expected: "failed to create \"nop/1\" processor, in pipeline \"metrics\": processor \"nop/1\" is not configured",
		},
		{
			name: "unknown_processor_factory",
			receiverCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopReceiverFactory.CreateDefaultConfig(),
			},
			processorCfgs: map[component.ID]component.Config{
				component.NewID("unknown"): nopProcessorFactory.CreateDefaultConfig(),
			},
			exporterCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopReceiverFactory.CreateDefaultConfig(),
			},
			pipelineCfgs: map[component.ID]*PipelineConfig{
				component.NewID("metrics"): {
					Receivers:  []component.ID{component.NewID("nop")},
					Processors: []component.ID{component.NewID("unknown")},
					Exporters:  []component.ID{component.NewID("nop")},
				},
			},
			expected: "failed to create \"unknown\" processor, in pipeline \"metrics\": processor factory not available for: \"unknown\"",
		},
		{
			name: "unknown_receiver_config",
			receiverCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopReceiverFactory.CreateDefaultConfig(),
			},
			exporterCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopReceiverFactory.CreateDefaultConfig(),
			},
			pipelineCfgs: map[component.ID]*PipelineConfig{
				component.NewID("logs"): {
					Receivers: []component.ID{component.NewID("nop"), component.NewIDWithName("nop", "1")},
					Exporters: []component.ID{component.NewID("nop")},
				},
			},
			expected: "failed to create \"nop/1\" receiver, in pipeline \"logs/*\": receiver \"nop/1\" is not configured",
		},
		{
			name: "unknown_receiver_factory",
			receiverCfgs: map[component.ID]component.Config{
				component.NewID("unknown"): nopReceiverFactory.CreateDefaultConfig(),
			},
			exporterCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopReceiverFactory.CreateDefaultConfig(),
			},
			pipelineCfgs: map[component.ID]*PipelineConfig{
				component.NewID("logs"): {
					Receivers: []component.ID{component.NewID("unknown")},
					Exporters: []component.ID{component.NewID("nop")},
				},
			},
			expected: "failed to create \"unknown\" receiver, in pipeline \"logs/*\": receiver factory not available for: \"unknown\"",
		},
		{
			name: "unknown_connector_factory",
			receiverCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopReceiverFactory.CreateDefaultConfig(),
			},
			exporterCfgs: map[component.ID]component.Config{
				component.NewID("nop"): nopReceiverFactory.CreateDefaultConfig(),
			},
			connectorCfgs: map[component.ID]component.Config{
				component.NewID("unknown"): nopConnectorFactory.CreateDefaultConfig(),
			},
			pipelineCfgs: map[component.ID]*PipelineConfig{
				component.NewIDWithName("traces", "in"): {
					Receivers: []component.ID{component.NewID("nop")},
					Exporters: []component.ID{component.NewID("unknown")},
				},
				component.NewIDWithName("traces", "out"): {
					Receivers: []component.ID{component.NewID("unknown")},
					Exporters: []component.ID{component.NewID("nop")},
				},
			},
			expected: "connector factory not available for: \"unknown\"",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			set := pipelinesSettings{
				BuildInfo: component.NewDefaultBuildInfo(),
				Telemetry: componenttest.NewNopTelemetrySettings(),
				ReceiverBuilder: receiver.NewBuilder(
					test.receiverCfgs,
					map[component.Type]receiver.Factory{
						nopReceiverFactory.Type(): nopReceiverFactory,
						badReceiverFactory.Type(): badReceiverFactory,
					}),
				ProcessorBuilder: processor.NewBuilder(
					test.processorCfgs,
					map[component.Type]processor.Factory{
						nopProcessorFactory.Type(): nopProcessorFactory,
						badProcessorFactory.Type(): badProcessorFactory,
					}),
				ExporterBuilder: exporter.NewBuilder(
					test.exporterCfgs,
					map[component.Type]exporter.Factory{
						nopExporterFactory.Type(): nopExporterFactory,
						badExporterFactory.Type(): badExporterFactory,
					}),
				ConnectorBuilder: connector.NewBuilder(
					test.connectorCfgs,
					map[component.Type]connector.Factory{
						nopConnectorFactory.Type(): nopConnectorFactory,
						badConnectorFactory.Type(): badConnectorFactory,
					}),
				PipelineConfigs: test.pipelineCfgs,
			}
			_, err := buildPipelinesGraph(context.Background(), set)
			if test.expected != "" {
				assert.EqualError(t, err, test.expected)
			} else {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), test.expectedStartsWith)
			}
		})
	}
}

// // This includes all tests from the previous implmentation, plus a new one
// // relevant only to the new graph-based implementation.
func TestGraphFailToStartAndShutdown(t *testing.T) {
	errReceiverFactory := newErrReceiverFactory()
	errProcessorFactory := newErrProcessorFactory()
	errExporterFactory := newErrExporterFactory()
	errConnectorFactory := newErrConnectorFactory()
	nopReceiverFactory := receivertest.NewNopFactory()
	nopProcessorFactory := processortest.NewNopFactory()
	nopExporterFactory := exportertest.NewNopFactory()
	nopConnectorFactory := connectortest.NewNopFactory()

	set := pipelinesSettings{
		Telemetry: componenttest.NewNopTelemetrySettings(),
		BuildInfo: component.NewDefaultBuildInfo(),
		ReceiverBuilder: receiver.NewBuilder(
			map[component.ID]component.Config{
				component.NewID(nopReceiverFactory.Type()): nopReceiverFactory.CreateDefaultConfig(),
				component.NewID(errReceiverFactory.Type()): errReceiverFactory.CreateDefaultConfig(),
			},
			map[component.Type]receiver.Factory{
				nopReceiverFactory.Type(): nopReceiverFactory,
				errReceiverFactory.Type(): errReceiverFactory,
			}),
		ProcessorBuilder: processor.NewBuilder(
			map[component.ID]component.Config{
				component.NewID(nopProcessorFactory.Type()): nopProcessorFactory.CreateDefaultConfig(),
				component.NewID(errProcessorFactory.Type()): errProcessorFactory.CreateDefaultConfig(),
			},
			map[component.Type]processor.Factory{
				nopProcessorFactory.Type(): nopProcessorFactory,
				errProcessorFactory.Type(): errProcessorFactory,
			}),
		ExporterBuilder: exporter.NewBuilder(
			map[component.ID]component.Config{
				component.NewID(nopExporterFactory.Type()): nopExporterFactory.CreateDefaultConfig(),
				component.NewID(errExporterFactory.Type()): errExporterFactory.CreateDefaultConfig(),
			},
			map[component.Type]exporter.Factory{
				nopExporterFactory.Type(): nopExporterFactory,
				errExporterFactory.Type(): errExporterFactory,
			}),
		ConnectorBuilder: connector.NewBuilder(
			map[component.ID]component.Config{
				component.NewIDWithName(nopConnectorFactory.Type(), "conn"): nopConnectorFactory.CreateDefaultConfig(),
				component.NewIDWithName(errConnectorFactory.Type(), "conn"): errConnectorFactory.CreateDefaultConfig(),
			},
			map[component.Type]connector.Factory{
				nopConnectorFactory.Type(): nopConnectorFactory,
				errConnectorFactory.Type(): errConnectorFactory,
			}),
	}

	dataTypes := []component.DataType{component.DataTypeTraces, component.DataTypeMetrics, component.DataTypeLogs}
	for _, dt := range dataTypes {
		t.Run(string(dt)+"/receiver", func(t *testing.T) {
			set.PipelineConfigs = map[component.ID]*PipelineConfig{
				component.NewID(dt): {
					Receivers:  []component.ID{component.NewID("nop"), component.NewID("err")},
					Processors: []component.ID{component.NewID("nop")},
					Exporters:  []component.ID{component.NewID("nop")},
				},
			}
			pipelines, err := buildPipelinesGraph(context.Background(), set)
			assert.NoError(t, err)
			assert.Error(t, pipelines.StartAll(context.Background(), componenttest.NewNopHost()))
			assert.Error(t, pipelines.ShutdownAll(context.Background()))
		})

		t.Run(string(dt)+"/processor", func(t *testing.T) {
			set.PipelineConfigs = map[component.ID]*PipelineConfig{
				component.NewID(dt): {
					Receivers:  []component.ID{component.NewID("nop")},
					Processors: []component.ID{component.NewID("nop"), component.NewID("err")},
					Exporters:  []component.ID{component.NewID("nop")},
				},
			}
			pipelines, err := buildPipelinesGraph(context.Background(), set)
			assert.NoError(t, err)
			assert.Error(t, pipelines.StartAll(context.Background(), componenttest.NewNopHost()))
			assert.Error(t, pipelines.ShutdownAll(context.Background()))
		})

		t.Run(string(dt)+"/exporter", func(t *testing.T) {
			set.PipelineConfigs = map[component.ID]*PipelineConfig{
				component.NewID(dt): {
					Receivers:  []component.ID{component.NewID("nop")},
					Processors: []component.ID{component.NewID("nop")},
					Exporters:  []component.ID{component.NewID("nop"), component.NewID("err")},
				},
			}
			pipelines, err := buildPipelinesGraph(context.Background(), set)
			assert.NoError(t, err)
			assert.Error(t, pipelines.StartAll(context.Background(), componenttest.NewNopHost()))
			assert.Error(t, pipelines.ShutdownAll(context.Background()))
		})

		for _, dt2 := range dataTypes {
			t.Run(string(dt)+"/"+string(dt2)+"/connector", func(t *testing.T) {
				set.PipelineConfigs = map[component.ID]*PipelineConfig{
					component.NewIDWithName(dt, "in"): {
						Receivers:  []component.ID{component.NewID("nop")},
						Processors: []component.ID{component.NewID("nop")},
						Exporters:  []component.ID{component.NewID("nop"), component.NewIDWithName("err", "conn")},
					},
					component.NewIDWithName(dt2, "out"): {
						Receivers:  []component.ID{component.NewID("nop"), component.NewIDWithName("err", "conn")},
						Processors: []component.ID{component.NewID("nop")},
						Exporters:  []component.ID{component.NewID("nop")},
					},
				}
				pipelines, err := buildPipelinesGraph(context.Background(), set)
				assert.NoError(t, err)
				assert.Error(t, pipelines.StartAll(context.Background(), componenttest.NewNopHost()))
				assert.Error(t, pipelines.ShutdownAll(context.Background()))
			})
		}
	}
}

func (g *pipelinesGraph) getReceivers() map[component.DataType]map[component.ID]component.Component {
	receiversMap := make(map[component.DataType]map[component.ID]component.Component)
	receiversMap[component.DataTypeTraces] = make(map[component.ID]component.Component)
	receiversMap[component.DataTypeMetrics] = make(map[component.ID]component.Component)
	receiversMap[component.DataTypeLogs] = make(map[component.ID]component.Component)

	for _, pg := range g.pipelines {
		for _, rcvrNode := range pg.receivers {
			rcvrOrConnNode := g.componentGraph.Node(rcvrNode.ID())
			rcvrNode, ok := rcvrOrConnNode.(*receiverNode)
			if !ok {
				continue
			}
			receiversMap[rcvrNode.pipelineType][rcvrNode.componentID] = rcvrNode.Component
		}
	}
	return receiversMap
}

// Calculates the expected number of receiver and exporter instances in the specified pipeline.
//
// Expect one instance of each receiver and exporter, unless it is a connector.
//
// For connectors:
// - Let E equal the number of pipeline types in which the connector is used as an exporter.
// - Let R equal the number of pipeline types in which the connector is used as a receiver.
//
// Within the graph as a whole, we expect E*R instances, i.e. one per combination of data types.
//
// However, within an individual pipeline, we expect:
// - E instances of the connector as a receiver.
// - R instances of the connector as a exporter.
func expectedInstances(m map[component.ID]*PipelineConfig, pID component.ID) (int, int) {
	var r, e int
	for _, rID := range m[pID].Receivers {
		if rID.Type() != "exampleconnector" {
			r++
			continue
		}

		// This is a connector. Count the pipeline types where it is an exporter.
		typeMap := map[component.DataType]bool{}
		for pID, pCfg := range m {
			for _, eID := range pCfg.Exporters {
				if eID == rID {
					typeMap[pID.Type()] = true
				}
			}
		}
		r += len(typeMap)
	}
	for _, eID := range m[pID].Exporters {
		if eID.Type() != "exampleconnector" {
			e++
			continue
		}

		// This is a connector. Count the pipeline types where it is a receiver.
		typeMap := map[component.DataType]bool{}
		for pID, pCfg := range m {
			for _, rID := range pCfg.Receivers {
				if rID == eID {
					typeMap[pID.Type()] = true
				}
			}
		}
		e += len(typeMap)
	}
	return r, e
}
