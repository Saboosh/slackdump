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

package export

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/rusq/fsadapter"
	"github.com/schollz/progressbar/v3"

	"github.com/rusq/slackdump/v4/cmd/slackdump/internal/bootstrap"
	"github.com/rusq/slackdump/v4/cmd/slackdump/internal/cfg"
	"github.com/rusq/slackdump/v4/internal/chunk"
	"github.com/rusq/slackdump/v4/internal/chunk/backend/dbase"
	"github.com/rusq/slackdump/v4/internal/chunk/control"
	"github.com/rusq/slackdump/v4/internal/client"
	"github.com/rusq/slackdump/v4/internal/convert/transform"
	"github.com/rusq/slackdump/v4/internal/convert/transform/fileproc"
	"github.com/rusq/slackdump/v4/internal/network"
	"github.com/rusq/slackdump/v4/internal/structures"
	"github.com/rusq/slackdump/v4/source"
	"github.com/rusq/slackdump/v4/stream"
)

// exportWithDB runs the export with the database backend.
func exportWithDB(ctx context.Context, sess client.Slack, fsa fsadapter.FS, list *structures.EntityList, params exportFlags) error {
	lg := cfg.Log

	tmpdir, err := os.MkdirTemp("", "slackdump-*")
	if err != nil {
		return err
	}

	lg.InfoContext(ctx, "temporary directory in use", "tmpdir", tmpdir)
	wconn, si, err := bootstrap.Database(tmpdir, "export")
	if err != nil {
		return err
	}
	defer wconn.Close()

	tmpdbp, err := dbase.New(ctx, wconn, si)
	if err != nil {
		return err
	}
	defer func() {
		if err := tmpdbp.Close(); err != nil {
			lg.ErrorContext(ctx, "unable to close database processor", "error", err)
		}
	}()
	src := source.DatabaseWithSource(tmpdbp.Source())
	if !lg.Enabled(ctx, slog.LevelDebug) {
		defer func() { _ = os.RemoveAll(tmpdir) }()
	}

	conv := transform.NewExpConverter(src, fsa, transform.ExpWithMsgUpdateFunc(fileproc.ExportTokenUpdateFn(params.ExportToken)))
	tf := transform.NewExportCoordinator(ctx, conv, transform.WithBufferSize(1000))
	defer tf.Close()

	// starting the downloader
	dlEnabled := cfg.WithFiles && params.ExportStorageType != source.STnone
	fdl := fileproc.NewDownloader(ctx, dlEnabled, sess, fsa, lg)
	fp := fileproc.NewExport(params.ExportStorageType, fdl)
	avdl := fileproc.NewDownloader(ctx, cfg.WithAvatars, sess, fsa, lg)
	avp := fileproc.NewAvatarProc(avdl)

	lg.InfoContext(ctx, "running export...")
	pb := bootstrap.ProgressBar(ctx, lg, progressbar.OptionShowCount()) // progress bar

	// Reset rate-limit counters so stats reflect only this export run.
	network.ResetRateLimitStats()

	// Running stats for progress output.
	type channelStats struct {
		name      string
		messages  int
		threads   int
		startTime time.Time
		endTime   time.Time
	}
	var (
		totalMessages  int
		totalThreads   int
		channelsSeen   = make(map[string]*channelStats) // channelID -> stats
		currentChannel string
	)

	s := stream.New(sess, cfg.Limits,
		stream.OptOldest(time.Time(cfg.Oldest)),
		stream.OptLatest(time.Time(cfg.Latest)),
		stream.OptFailOnNonCritError(cfg.FailOnNonCritical),
		stream.OptResultFn(func(sr stream.Result) error {
			lg.DebugContext(ctx, "conversations", "sr", sr.String())

			now := time.Now()
			totalMessages += sr.MessageCount

			switch sr.Type {
			case stream.RTChannel:
				currentChannel = sr.ChannelID
				cs, ok := channelsSeen[sr.ChannelID]
				if !ok {
					cs = &channelStats{name: sr.ChannelName, startTime: now}
					channelsSeen[sr.ChannelID] = cs
				}
				cs.messages += sr.MessageCount
				cs.threads += sr.ThreadCount
				cs.endTime = now
				totalThreads += sr.ThreadCount
				elapsed := now.Sub(cs.startTime).Round(time.Millisecond)
				desc := fmt.Sprintf("<%s> (%d msgs, %d threads, %s) | total: %d msgs, %d channels",
					sr.ChannelID,
					cs.messages,
					cs.threads,
					elapsed,
					totalMessages,
					len(channelsSeen),
				)
				pb.Describe(desc)
			case stream.RTThread:
				if cs, ok := channelsSeen[sr.ChannelID]; ok {
					cs.endTime = now
				}
				desc := fmt.Sprintf("<%s> thread (%d replies) | total: %d msgs, %d channels",
					currentChannel,
					sr.MessageCount,
					totalMessages,
					len(channelsSeen),
				)
				pb.Describe(desc)
			default:
				pb.Describe(sr.String())
			}

			_ = pb.Add(1)
			return nil
		}),
	)

	flags := control.Flags{
		MemberOnly:    cfg.MemberOnly,
		RecordFiles:   false, // archive format is transitory, don't need extra info.
		ChannelUsers:  cfg.OnlyChannelUsers,
		ChannelTypes:  cfg.ChannelTypes,
		IncludeLabels: cfg.IncludeCustomLabels,
	}
	ctr, err := control.New(
		ctx,
		s,
		tmpdbp,
		control.WithFiler(fp),
		control.WithLogger(lg),
		control.WithFlags(flags),
		control.WithCoordinator(tf),
		control.WithAvatarProcessor(avp),
	)
	if err != nil {
		return fmt.Errorf("error creating db controller: %w", err)
	}
	defer ctr.Close()

	streamStart := time.Now()
	if err := ctr.Run(ctx, list); err != nil {
		_ = pb.Finish()
		return err
	}
	streamMs := time.Since(streamStart).Milliseconds()
	_ = pb.Finish()

	// at this point no goroutines are running, we are safe to assume that
	// everything we need is in the chunk directory.
	writeIndexStart := time.Now()
	if err := conv.WriteIndex(ctx); err != nil {
		return err
	}
	writeIndexMs := time.Since(writeIndexStart).Milliseconds()
	lg.Debug("index written")

	finalizeStart := time.Now()
	if err := tf.Close(); err != nil {
		return err
	}
	finalizeMs := time.Since(finalizeStart).Milliseconds()

	pb.Describe("OK")

	rlHits, rlWaitMs := network.RateLimitStats()
	lg.InfoContext(ctx, "conversations export finished",
		"total_messages", totalMessages,
		"total_threads", totalThreads,
		"total_channels", len(channelsSeen),
		"stream_ms", streamMs,
		"write_index_ms", writeIndexMs,
		"finalize_ms", finalizeMs,
		"rate_limit_hits", rlHits,
		"rate_limit_wait_ms", rlWaitMs,
	)

	// Write per-run channel activity to JSON for historical tracking.
	type channelActivity struct {
		ID         string `json:"id"`
		Name       string `json:"name,omitempty"`
		Messages   int    `json:"messages"`
		Threads    int    `json:"threads"`
		DurationMs int64  `json:"duration_ms,omitempty"`
	}
	activity := make([]channelActivity, 0, len(channelsSeen))
	var emptyCount int
	for id, cs := range channelsSeen {
		var durMs int64
		if !cs.startTime.IsZero() && !cs.endTime.IsZero() {
			durMs = cs.endTime.Sub(cs.startTime).Milliseconds()
		}
		activity = append(activity, channelActivity{
			ID:         id,
			Name:       cs.name,
			Messages:   cs.messages,
			Threads:    cs.threads,
			DurationMs: durMs,
		})
		if cs.messages == 0 {
			emptyCount++
		}
	}
	sort.Slice(activity, func(i, j int) bool {
		return activity[i].Name < activity[j].Name
	})

	// Log top-5 slowest channels for quick bottleneck identification.
	if len(activity) > 0 {
		byDur := make([]channelActivity, len(activity))
		copy(byDur, activity)
		sort.Slice(byDur, func(i, j int) bool {
			return byDur[i].DurationMs > byDur[j].DurationMs
		})
		n := 5
		if len(byDur) < n {
			n = len(byDur)
		}
		lg.InfoContext(ctx, "slowest channels (by duration)")
		for i := range n {
			ch := byDur[i]
			lg.InfoContext(ctx, "  slow channel",
				"rank", i+1,
				"name", ch.Name,
				"id", ch.ID,
				"messages", ch.Messages,
				"threads", ch.Threads,
				"duration_ms", ch.DurationMs,
			)
		}
	}

	activityData := struct {
		Timestamp  string `json:"timestamp"`
		TotalMs    int64  `json:"total_stream_ms"`
		Phases     struct {
			StreamMs     int64 `json:"stream_ms"`
			WriteIndexMs int64 `json:"write_index_ms"`
			FinalizeMs   int64 `json:"finalize_ms"`
		} `json:"phases"`
		RateLimits struct {
			Hits        int64 `json:"hits"`
			TotalWaitMs int64 `json:"total_wait_ms"`
		} `json:"rate_limits"`
		Channels []channelActivity `json:"channels"`
	}{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		TotalMs:   streamMs + writeIndexMs + finalizeMs,
		Channels:  activity,
	}
	activityData.Phases.StreamMs = streamMs
	activityData.Phases.WriteIndexMs = writeIndexMs
	activityData.Phases.FinalizeMs = finalizeMs
	activityData.RateLimits.Hits = rlHits
	activityData.RateLimits.TotalWaitMs = rlWaitMs

	const activityFile = "channel_activity.json"
	if activityJSON, err := json.MarshalIndent(activityData, "", "  "); err != nil {
		lg.ErrorContext(ctx, "failed to marshal channel activity", "error", err)
	} else if err := os.WriteFile(activityFile, activityJSON, 0644); err != nil {
		lg.ErrorContext(ctx, "failed to write channel activity file", "error", err)
	} else {
		lg.InfoContext(ctx, "channel activity written",
			"file", activityFile,
			"total", len(activity),
			"empty", emptyCount,
		)
	}

	// Also write the simple exclusion-ready file for convenience.
	if emptyCount > 0 {
		var sb strings.Builder
		sb.WriteString("# Channels with no messages — copy lines into your exclusion file\n")
		sb.WriteString("# Generated by slackdump export\n")
		for _, ch := range activity {
			if ch.Messages > 0 {
				continue
			}
			if ch.Name != "" {
				fmt.Fprintf(&sb, "^%s  # %s\n", ch.ID, ch.Name)
			} else {
				fmt.Fprintf(&sb, "^%s\n", ch.ID)
			}
		}
		const excludeFile = "empty_channels.txt"
		if err := os.WriteFile(excludeFile, []byte(sb.String()), 0644); err != nil {
			lg.ErrorContext(ctx, "failed to write empty channels file", "error", err)
		}
	}

	lg.DebugContext(ctx, "chunk files retained", "dir", tmpdir)
	return nil
}

// exportWithDir runs the export with the chunk file directory backend.  It
// exists as a fallback in case database backend has issues.
//
// Deprecated: use exportWithDB instead.
func exportWithDir(ctx context.Context, sess client.Slack, fsa fsadapter.FS, list *structures.EntityList, params exportFlags) error {
	lg := cfg.Log

	tmpdir, err := os.MkdirTemp("", "slackdump-*")
	if err != nil {
		return err
	}

	lg.InfoContext(ctx, "temporary directory in use", "tmpdir", tmpdir)
	chunkdir, err := chunk.OpenDir(tmpdir)
	if err != nil {
		return err
	}
	defer chunkdir.Close()
	if !lg.Enabled(ctx, slog.LevelDebug) {
		defer func() { _ = chunkdir.RemoveAll() }()
	}
	src := source.OpenChunkDir(chunkdir, true)
	conv := transform.NewExpConverter(src, fsa, transform.ExpWithMsgUpdateFunc(fileproc.ExportTokenUpdateFn(params.ExportToken)))
	tf := transform.NewExportCoordinator(ctx, conv, transform.WithBufferSize(1000))
	defer tf.Close()

	// starting the downloader
	dlEnabled := cfg.WithFiles && params.ExportStorageType != source.STnone
	fdl := fileproc.NewDownloader(ctx, dlEnabled, sess, fsa, lg)
	fp := fileproc.NewExport(params.ExportStorageType, fdl)
	avdl := fileproc.NewDownloader(ctx, cfg.WithAvatars, sess, fsa, lg)
	avp := fileproc.NewAvatarProc(avdl)

	lg.InfoContext(ctx, "running export...")
	pb := bootstrap.ProgressBar(ctx, lg, progressbar.OptionShowCount()) // progress bar

	stream := stream.New(sess, cfg.Limits,
		stream.OptOldest(time.Time(cfg.Oldest)),
		stream.OptLatest(time.Time(cfg.Latest)),
		stream.OptFailOnNonCritError(cfg.FailOnNonCritical),
		stream.OptResultFn(func(sr stream.Result) error {
			lg.DebugContext(ctx, "conversations", "sr", sr.String())
			pb.Describe(sr.String())
			_ = pb.Add(1)
			return nil
		}),
	)

	flags := control.Flags{
		MemberOnly:    cfg.MemberOnly,
		RecordFiles:   false, // archive format is transitory, don't need extra info.
		ChannelUsers:  cfg.OnlyChannelUsers,
		IncludeLabels: cfg.IncludeCustomLabels,
		ChannelTypes:  cfg.ChannelTypes,
	}
	ctr := control.NewDir(
		chunkdir,
		stream,
		control.WithFiler(fp),
		control.WithLogger(lg),
		control.WithFlags(flags),
		control.WithCoordinator(tf),
		control.WithAvatarProcessor(avp),
	)
	defer ctr.Close()

	if err := ctr.Run(ctx, list); err != nil {
		_ = pb.Finish()
		return err
	}
	_ = pb.Finish()
	// at this point no goroutines are running, we are safe to assume that
	// everything we need is in the chunk directory.
	if err := conv.WriteIndex(ctx); err != nil {
		return err
	}
	lg.Debug("index written")
	if err := tf.Close(); err != nil {
		return err
	}
	pb.Describe("OK")
	lg.InfoContext(ctx, "conversations export finished")
	lg.DebugContext(ctx, "chunk files retained", "dir", tmpdir)
	return nil
}
