// Copyright (c) 2021-2026 Rustam Gilyazov and Contributors.
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package stream

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rusq/slack"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"

	"github.com/rusq/slackdump/v4/internal/client/mock_client"
	"github.com/rusq/slackdump/v4/internal/fixtures"
	"github.com/rusq/slackdump/v4/internal/network"
	"github.com/rusq/slackdump/v4/internal/structures"
	"github.com/rusq/slackdump/v4/mocks/mock_processor"
)

func TestStream_canvas(t *testing.T) {
	testChannel := fixtures.Load[[]*slack.Channel](fixtures.TestChannelsJSON)[0]
	type args struct {
		ctx context.Context
		// proc    processor.Conversations
		channel *slack.Channel
		fileId  string
	}
	tests := []struct {
		name     string
		fields   *Stream
		args     args
		expectFn func(ms *mock_client.MockSlack, mc *mock_processor.MockConversations)
		wantErr  bool
	}{
		{
			name:   "file ID is empty",
			fields: &Stream{},
			args: args{
				ctx:     t.Context(),
				channel: &slack.Channel{},
				fileId:  "",
			},
			wantErr: false,
		},
		{
			name:   "getfileinfocontext returns an error",
			fields: &Stream{},
			args: args{
				ctx:    t.Context(),
				fileId: "F123456",
			},
			expectFn: func(ms *mock_client.MockSlack, mc *mock_processor.MockConversations) {
				ms.EXPECT().GetFileInfoContext(gomock.Any(), "F123456", 0, 1).Return(nil, nil, nil, errors.New("getfileinfocontext error"))
			},
			wantErr: true,
		},
		{
			name:   "file not found",
			fields: &Stream{},
			args: args{
				ctx:    t.Context(),
				fileId: "F123456",
			},
			expectFn: func(ms *mock_client.MockSlack, mc *mock_processor.MockConversations) {
				ms.EXPECT().GetFileInfoContext(gomock.Any(), "F123456", 0, 1).Return(nil, nil, nil, nil)
			},
			wantErr: true,
		},
		{
			name:   "success",
			fields: &Stream{},
			args: args{
				ctx:     t.Context(),
				channel: testChannel,
				fileId:  "F123456",
			},
			expectFn: func(ms *mock_client.MockSlack, mc *mock_processor.MockConversations) {
				ms.EXPECT().
					GetFileInfoContext(gomock.Any(), "F123456", 0, 1).
					Return(&slack.File{ID: "F123456"}, nil, nil, nil)
				mc.EXPECT().
					Files(gomock.Any(), testChannel, slack.Message{}, []slack.File{{ID: "F123456"}}).
					Return(nil)
			},
			wantErr: false,
		},
		{
			name:   "processor returns an error",
			fields: &Stream{},
			args: args{
				ctx:     t.Context(),
				channel: testChannel,
				fileId:  "F123456",
			},
			expectFn: func(ms *mock_client.MockSlack, mc *mock_processor.MockConversations) {
				ms.EXPECT().
					GetFileInfoContext(gomock.Any(), "F123456", 0, 1).
					Return(&slack.File{ID: "F123456"}, nil, nil, nil)
				mc.EXPECT().
					Files(gomock.Any(), testChannel, slack.Message{}, []slack.File{{ID: "F123456"}}).
					Return(assert.AnError)
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			ms := mock_client.NewMockSlack(ctrl)
			mc := mock_processor.NewMockConversations(ctrl)
			if tt.expectFn != nil {
				tt.expectFn(ms, mc)
			}
			cs := tt.fields
			cs.client = ms
			if err := cs.canvas(tt.args.ctx, mc, tt.args.channel, tt.args.fileId); (err != nil) != tt.wantErr {
				t.Errorf("Stream.canvas() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestStream_thread_does_not_apply_global_time_filter(t *testing.T) {
	// Verify that the thread function does NOT fall back to the stream's
	// global oldest/latest when the request has zero time bounds. This is
	// critical for threads discovered during time-filtered channel exports:
	// their replies should be fetched without time constraints.
	ctrl := gomock.NewController(t)
	ms := mock_client.NewMockSlack(ctrl)

	streamOldest := time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)
	streamLatest := time.Date(2026, 3, 11, 0, 0, 0, 0, time.UTC)

	cs := &Stream{
		client:    ms,
		oldest:    streamOldest,
		latest:    streamLatest,
		limits:    limits(network.NoLimits),
		chanCache: new(chanCache),
	}

	threadTS := "1710000000.000000"
	req := request{
		sl: &structures.SlackLink{
			Channel:  "C123",
			ThreadTS: threadTS,
		},
		// Oldest/Latest intentionally zero — simulates a thread discovered
		// during channel scanning (procChanMsg doesn't set these).
	}

	replyMsg := slack.Message{Msg: slack.Msg{
		Timestamp:       threadTS,
		ThreadTimestamp: threadTS,
		Text:            "parent message",
	}}

	// The key assertion: Oldest and Latest in the API call should be empty
	// strings (not the stream's global time filter).
	ms.EXPECT().
		GetConversationRepliesContext(gomock.Any(), gomock.Cond(func(x any) bool {
			params, ok := x.(*slack.GetConversationRepliesParameters)
			if !ok {
				return false
			}
			return params.Oldest == "" && params.Latest == "" &&
				params.ChannelID == "C123" && params.Timestamp == threadTS
		})).
		Return([]slack.Message{replyMsg}, false, "", nil)

	var callbackCalled bool
	err := cs.thread(t.Context(), req, func(mm []slack.Message, isLast bool) error {
		callbackCalled = true
		assert.Len(t, mm, 1)
		assert.True(t, isLast)
		return nil
	})

	assert.NoError(t, err)
	assert.True(t, callbackCalled, "callback should have been called")
}

func TestStream_thread_respects_explicit_request_time_bounds(t *testing.T) {
	// Verify that when a request has explicit Oldest/Latest (e.g., user-
	// requested thread with time bounds), those are passed to the API.
	ctrl := gomock.NewController(t)
	ms := mock_client.NewMockSlack(ctrl)

	cs := &Stream{
		client:    ms,
		oldest:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		latest:    time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC),
		limits:    limits(network.NoLimits),
		chanCache: new(chanCache),
	}

	reqOldest := time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)
	reqLatest := time.Date(2026, 3, 11, 0, 0, 0, 0, time.UTC)
	threadTS := "1710000000.000000"

	req := request{
		sl: &structures.SlackLink{
			Channel:  "C123",
			ThreadTS: threadTS,
		},
		threadOnly: true,
		Oldest:     reqOldest,
		Latest:     reqLatest,
	}

	replyMsg := slack.Message{Msg: slack.Msg{
		Timestamp:       threadTS,
		ThreadTimestamp: threadTS,
	}}

	expectedOldest := structures.FormatSlackTS(reqOldest)
	expectedLatest := structures.FormatSlackTS(reqLatest)

	ms.EXPECT().
		GetConversationRepliesContext(gomock.Any(), gomock.Cond(func(x any) bool {
			params, ok := x.(*slack.GetConversationRepliesParameters)
			if !ok {
				return false
			}
			// Should use the request's time bounds, NOT the stream's.
			return params.Oldest == expectedOldest && params.Latest == expectedLatest
		})).
		Return([]slack.Message{replyMsg}, false, "", nil)

	err := cs.thread(t.Context(), req, func(mm []slack.Message, isLast bool) error {
		return nil
	})
	assert.NoError(t, err)
}
