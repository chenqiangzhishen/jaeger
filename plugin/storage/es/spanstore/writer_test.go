// Copyright (c) 2017 Uber Technologies, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package spanstore

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"github.com/uber/jaeger-lib/metrics/metricstest"

	"github.com/jaegertracing/jaeger/model"
	"github.com/jaegertracing/jaeger/pkg/es/mocks"
	"github.com/jaegertracing/jaeger/pkg/testutils"
	"github.com/jaegertracing/jaeger/plugin/storage/es/spanstore/dbmodel"
	"github.com/jaegertracing/jaeger/storage/spanstore"
)

type spanWriterTest struct {
	client    *mocks.Client
	logger    *zap.Logger
	logBuffer *testutils.Buffer
	writer    *SpanWriter
}

func withSpanWriter(fn func(w *spanWriterTest)) {
	client := &mocks.Client{}
	logger, logBuffer := testutils.NewLogger()
	metricsFactory := metricstest.NewFactory(0)
	w := &spanWriterTest{
		client:    client,
		logger:    logger,
		logBuffer: logBuffer,
		writer:    NewSpanWriter(SpanWriterParams{Client: client, Logger: logger, MetricsFactory: metricsFactory}),
	}
	fn(w)
}

var _ spanstore.Writer = &SpanWriter{} // check API conformance

func TestSpanWriterIndices(t *testing.T) {
	client := &mocks.Client{}
	logger, _ := testutils.NewLogger()
	metricsFactory := metricstest.NewFactory(0)
	date := time.Now()
	dateFormat := date.UTC().Format("2006-01-02")
	testCases := []struct {
		indices []string
		params  SpanWriterParams
	}{
		{params:SpanWriterParams{Client: client, Logger: logger, MetricsFactory: metricsFactory,
			IndexPrefix: "", Archive: false},
			indices:[]string{spanIndex+dateFormat, serviceIndex+dateFormat}},
		{params:SpanWriterParams{Client: client, Logger: logger, MetricsFactory: metricsFactory,
			IndexPrefix: "foo:", Archive: false},
			indices:[]string{"foo:"+indexPrefixSeparator+spanIndex+dateFormat, "foo:"+indexPrefixSeparator+serviceIndex+dateFormat}},
		{params:SpanWriterParams{Client: client, Logger: logger, MetricsFactory: metricsFactory,
			IndexPrefix: "", Archive: true},
			indices:[]string{spanIndex+archiveIndexSuffix, ""}},
		{params:SpanWriterParams{Client: client, Logger: logger, MetricsFactory: metricsFactory,
			IndexPrefix: "foo:", Archive: true},
			indices:[]string{"foo:"+indexPrefixSeparator+spanIndex+archiveIndexSuffix, ""}},
		{params:SpanWriterParams{Client: client, Logger: logger, MetricsFactory: metricsFactory,
			IndexPrefix: "foo:", Archive: true, UseReadWriteAliases: true},
			indices:[]string{"foo:"+indexPrefixSeparator+spanIndex+archiveWriteIndexSuffix, ""}},
	}
	for _, testCase := range testCases {
		w := NewSpanWriter(testCase.params)
		spanIndexName, serviceIndexName := w.spanServiceIndex(date)
		assert.Equal(t, testCase.indices, []string{spanIndexName, serviceIndexName})
	}
}

func TestClientClose(t *testing.T) {
	withSpanWriter(func(w *spanWriterTest) {
		w.client.On("Close").Return(nil)
		w.writer.Close()
		w.client.AssertNumberOfCalls(t, "Close", 1)
	})
}

// This test behaves as a large test that checks WriteSpan's behavior as a whole.
// Extra tests for individual functions are below.
func TestSpanWriter_WriteSpan(t *testing.T) {
	testCases := []struct {
		caption                 string
		serviceIndexExists      bool
		spanIndexExists         bool
		serviceIndexCreateError error
		spanIndexCreateError    error
		servicePutError         error
		spanPutError            error
		expectedError           string
		expectedLogs            []string
	}{
		{
			caption: "index creation error",

			serviceIndexExists: false,

			serviceIndexCreateError: errors.New("index creation error"),
			expectedError:           "Failed to create index: index creation error",
			expectedLogs: []string{
				`"msg":"Failed to create index"`,
				`"trace_id":"1"`,
				`"span_id":"0"`,
				`"error":"index creation error"`,
			},
		},
		{
			caption: "span insertion error",

			serviceIndexExists: false,

			expectedError: "",
			expectedLogs:  []string{},
		},
		{
			caption: "span index dne error",

			serviceIndexExists: true,
			spanIndexExists:    false,

			spanIndexCreateError: errors.New("span index creation error"),
			expectedError:        "Failed to create index: span index creation error",
			expectedLogs: []string{
				`"msg":"Failed to create index"`,
				`"trace_id":"1"`,
				`"span_id":"0"`,
				`"error":"span index creation error"`,
			},
		},
	}
	for _, tc := range testCases {
		testCase := tc
		t.Run(testCase.caption, func(t *testing.T) {
			withSpanWriter(func(w *spanWriterTest) {
				date, err := time.Parse(time.RFC3339, "1995-04-21T22:08:41+00:00")
				require.NoError(t, err)

				span := &model.Span{
					TraceID:       model.NewTraceID(0, 1),
					SpanID:        model.NewSpanID(0),
					OperationName: "operation",
					Process: &model.Process{
						ServiceName: "service",
					},
					StartTime: date,
				}

				spanIndexName := "jaeger-span-1995-04-21"
				serviceIndexName := "jaeger-service-1995-04-21"
				serviceHash := "de3b5a8f1a79989d"

				serviceExistsService := &mocks.IndicesExistsService{}
				spanExistsService := &mocks.IndicesExistsService{}

				serviceExistsService.On("Do", mock.AnythingOfType("*context.emptyCtx")).Return(testCase.serviceIndexExists, nil)
				spanExistsService.On("Do", mock.AnythingOfType("*context.emptyCtx")).Return(testCase.spanIndexExists, nil)

				serviceCreateService := &mocks.IndicesCreateService{}
				serviceCreateService.On("Body", stringMatcher(w.writer.fixMapping(serviceMapping))).Return(serviceCreateService)
				serviceCreateService.On("Do", mock.AnythingOfType("*context.emptyCtx")).Return(nil, testCase.serviceIndexCreateError)

				spanCreateService := &mocks.IndicesCreateService{}
				spanCreateService.On("Body", stringMatcher(w.writer.fixMapping(spanMapping))).Return(spanCreateService)
				spanCreateService.On("Do", mock.AnythingOfType("*context.emptyCtx")).Return(nil, testCase.spanIndexCreateError)

				indexService := &mocks.IndexService{}
				indexServicePut := &mocks.IndexService{}
				indexSpanPut := &mocks.IndexService{}

				indexService.On("Index", stringMatcher(spanIndexName)).Return(indexService)
				indexService.On("Index", stringMatcher(serviceIndexName)).Return(indexService)

				indexService.On("Type", stringMatcher(serviceType)).Return(indexServicePut)
				indexService.On("Type", stringMatcher(spanType)).Return(indexSpanPut)

				indexServicePut.On("Id", stringMatcher(serviceHash)).Return(indexServicePut)
				indexServicePut.On("BodyJson", mock.AnythingOfType("dbmodel.Service")).Return(indexServicePut)
				indexServicePut.On("Add")

				indexSpanPut.On("Id", mock.AnythingOfType("string")).Return(indexSpanPut)
				indexSpanPut.On("BodyJson", mock.AnythingOfType("**dbmodel.Span")).Return(indexSpanPut)
				indexSpanPut.On("Add")

				w.client.On("IndexExists", stringMatcher(spanIndexName)).Return(spanExistsService)
				w.client.On("CreateIndex", stringMatcher(spanIndexName)).Return(spanCreateService)
				w.client.On("IndexExists", stringMatcher(serviceIndexName)).Return(serviceExistsService)
				w.client.On("CreateIndex", stringMatcher(serviceIndexName)).Return(serviceCreateService)
				w.client.On("Index").Return(indexService)

				err = w.writer.WriteSpan(span)

				if testCase.expectedError == "" {
					require.NoError(t, err)
					indexServicePut.AssertNumberOfCalls(t, "Add", 1)
					indexSpanPut.AssertNumberOfCalls(t, "Add", 1)
				} else {
					assert.EqualError(t, err, testCase.expectedError)
				}

				for _, expectedLog := range testCase.expectedLogs {
					assert.True(t, strings.Contains(w.logBuffer.String(), expectedLog), "Log must contain %s, but was %s", expectedLog, w.logBuffer.String())
				}
				if len(testCase.expectedLogs) == 0 {
					assert.Equal(t, "", w.logBuffer.String())
				}
			})
		})
	}
}

func TestSpanIndexName(t *testing.T) {
	date, err := time.Parse(time.RFC3339, "1995-04-21T22:08:41+00:00")
	require.NoError(t, err)
	span := &model.Span{
		StartTime: date,
	}
	spanIndexName := indexWithDate(spanIndex, span.StartTime)
	serviceIndexName := indexWithDate(serviceIndex, span.StartTime)
	assert.Equal(t, "jaeger-span-1995-04-21", spanIndexName)
	assert.Equal(t, "jaeger-service-1995-04-21", serviceIndexName)
}

func TestFixMapping(t *testing.T) {
	withSpanWriter(func(w *spanWriterTest) {
		testMapping := `{
		   "settings":{
		      "index.number_of_shards": ${__NUMBER_OF_SHARDS__},
      		      "index.number_of_replicas": ${__NUMBER_OF_REPLICAS__},
		      "index.mapping.nested_fields.limit":50,
		      "index.requests.cache.enable":true,
		      "index.mapper.dynamic":false
		   },
		   "mappings":{
		      "_default_":{
			 "_all":{
			    "enabled":false
			 }
		      }
		   }
		}`
		expectedMapping := `{
		   "settings":{
		      "index.number_of_shards": 5,
      		      "index.number_of_replicas": 0,
		      "index.mapping.nested_fields.limit":50,
		      "index.requests.cache.enable":true,
		      "index.mapper.dynamic":false
		   },
		   "mappings":{
		      "_default_":{
			 "_all":{
			    "enabled":false
			 }
		      }
		   }
		}`

		assert.Equal(t, expectedMapping, w.writer.fixMapping(testMapping))
	})
}

func TestWriteSpanInternal(t *testing.T) {
	withSpanWriter(func(w *spanWriterTest) {
		indexService := &mocks.IndexService{}

		indexName := "jaeger-1995-04-21"
		indexService.On("Index", stringMatcher(indexName)).Return(indexService)
		indexService.On("Type", stringMatcher(spanType)).Return(indexService)
		indexService.On("BodyJson", mock.AnythingOfType("**dbmodel.Span")).Return(indexService)
		indexService.On("Add")

		w.client.On("Index").Return(indexService)

		jsonSpan := &dbmodel.Span{}

		w.writer.writeSpan(indexName, jsonSpan)
		indexService.AssertNumberOfCalls(t, "Add", 1)
		assert.Equal(t, "", w.logBuffer.String())
	})
}

func TestWriteSpanInternalError(t *testing.T) {
	withSpanWriter(func(w *spanWriterTest) {
		indexService := &mocks.IndexService{}

		indexName := "jaeger-1995-04-21"
		indexService.On("Index", stringMatcher(indexName)).Return(indexService)
		indexService.On("Type", stringMatcher(spanType)).Return(indexService)
		indexService.On("BodyJson", mock.AnythingOfType("**dbmodel.Span")).Return(indexService)
		indexService.On("Add")

		w.client.On("Index").Return(indexService)

		jsonSpan := &dbmodel.Span{
			TraceID: dbmodel.TraceID("1"),
			SpanID:  dbmodel.SpanID("0"),
		}

		w.writer.writeSpan(indexName, jsonSpan)
		indexService.AssertNumberOfCalls(t, "Add", 1)
	})
}

func TestNewSpanTags(t *testing.T) {
	client := &mocks.Client{}
	logger, _ := testutils.NewLogger()
	metricsFactory := metricstest.NewFactory(0)
	testCases := []struct {
		writer   *SpanWriter
		expected dbmodel.Span
		name     string
	}{
		{
			writer: NewSpanWriter(SpanWriterParams{Client: client, Logger: logger, MetricsFactory: metricsFactory,
				AllTagsAsFields: true}),
			expected: dbmodel.Span{Tag: map[string]interface{}{"foo": "bar"}, Tags: []dbmodel.KeyValue{},
				Process: dbmodel.Process{Tag: map[string]interface{}{"bar": "baz"}, Tags: []dbmodel.KeyValue{}}},
			name: "allTagsAsFields",
		},
		{
			writer: NewSpanWriter(SpanWriterParams{Client: client, Logger: logger, MetricsFactory: metricsFactory,
				TagKeysAsFields: []string{"foo", "bar", "rere"}}),
			expected: dbmodel.Span{Tag: map[string]interface{}{"foo": "bar"}, Tags: []dbmodel.KeyValue{},
				Process: dbmodel.Process{Tag: map[string]interface{}{"bar": "baz"}, Tags: []dbmodel.KeyValue{}}},
			name: "definedTagNames",
		},
		{
			writer: NewSpanWriter(SpanWriterParams{Client: client, Logger: logger, MetricsFactory: metricsFactory}),
			expected: dbmodel.Span{
				Tags: []dbmodel.KeyValue{{
					Key:   "foo",
					Type:  dbmodel.StringType,
					Value: "bar",
				}},
				Process: dbmodel.Process{Tags: []dbmodel.KeyValue{{
					Key:   "bar",
					Type:  dbmodel.StringType,
					Value: "baz",
				}}}},
			name: "noAllTagsAsFields",
		},
	}

	s := &model.Span{Tags: []model.KeyValue{{Key: "foo", VStr: "bar"}},
		Process: &model.Process{Tags: []model.KeyValue{{Key: "bar", VStr: "baz"}}}}
	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			mSpan := test.writer.spanConverter.FromDomainEmbedProcess(s)
			assert.Equal(t, test.expected.Tag, mSpan.Tag)
			assert.Equal(t, test.expected.Tags, mSpan.Tags)
			assert.Equal(t, test.expected.Process.Tag, mSpan.Process.Tag)
			assert.Equal(t, test.expected.Process.Tags, mSpan.Process.Tags)
		})
	}
}

// stringMatcher can match a string argument when it contains a specific substring q
func stringMatcher(q string) interface{} {
	matchFunc := func(s string) bool {
		return strings.Contains(s, q)
	}
	return mock.MatchedBy(matchFunc)
}
