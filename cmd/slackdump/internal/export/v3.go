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

	// Running stats for progress output.
	type channelStats struct {
		name     string
		messages int
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

			totalMessages += sr.MessageCount

			switch sr.Type {
			case stream.RTChannel:
				currentChannel = sr.ChannelID
				cs, ok := channelsSeen[sr.ChannelID]
				if !ok {
					cs = &channelStats{name: sr.ChannelName}
					channelsSeen[sr.ChannelID] = cs
				}
				cs.messages += sr.MessageCount
				totalThreads += sr.ThreadCount
				desc := fmt.Sprintf("<%s> (%d msgs, %d threads) | total: %d msgs, %d channels",
					sr.ChannelID,
					cs.messages,
					sr.ThreadCount,
					totalMessages,
					len(channelsSeen),
				)
				pb.Describe(desc)
			case stream.RTThread:
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
	lg.InfoContext(ctx, "conversations export finished",
		"total_messages", totalMessages,
		"total_threads", totalThreads,
		"total_channels", len(channelsSeen),
	)

	// Report channels with no messages (candidates for exclusion list).
	var emptyChannels []struct{ id, name string }
	for id, cs := range channelsSeen {
		if cs.messages == 0 {
			emptyChannels = append(emptyChannels, struct{ id, name string }{id, cs.name})
		}
	}
	if len(emptyChannels) > 0 {
		sort.Slice(emptyChannels, func(i, j int) bool {
			return emptyChannels[i].name < emptyChannels[j].name
		})

		const excludeFile = "empty_channels.txt"
		var sb strings.Builder
		sb.WriteString("# Channels with no messages — copy lines into your exclusion file\n")
		sb.WriteString("# Generated by slackdump export\n")
		for _, ch := range emptyChannels {
			if ch.name != "" {
				fmt.Fprintf(&sb, "^%s  # %s\n", ch.id, ch.name)
			} else {
				fmt.Fprintf(&sb, "^%s\n", ch.id)
			}
		}
		if err := os.WriteFile(excludeFile, []byte(sb.String()), 0644); err != nil {
			lg.ErrorContext(ctx, "failed to write empty channels file", "error", err)
		} else {
			lg.InfoContext(ctx, "channels with no messages written to file",
				"file", excludeFile,
				"count", len(emptyChannels),
				"total_scanned", len(channelsSeen),
			)
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
