package main

import (
	"image"
	"os"
	"runtime"
	"time"

	tea "charm.land/bubbletea/v2"
	"golang.org/x/term"

	"github.com/gammons/slk/internal/config"
	"github.com/gammons/slk/internal/core"
	"github.com/gammons/slk/internal/debuglog"
	emojiwidth "github.com/gammons/slk/internal/emoji"
	imgpkg "github.com/gammons/slk/internal/image"
	"github.com/gammons/slk/internal/ui"
	"github.com/gammons/slk/internal/ui/imgrender"
	"github.com/gammons/slk/internal/ui/messages"
)

// terminalImages is what setupTerminalImages settled on: the protocol
// inline images and emoji draw with, and what the tea.Program needs to
// carry it.
type terminalImages struct {
	// Proto is the image protocol after detection and the probes.
	Proto imgpkg.Protocol
	// ImgCtx builds the messages pane's inline-image context. SendMsg
	// is send, which is nil until the tea.Program exists.
	ImgCtx func(send func(tea.Msg)) imgrender.ImageContext
	// PlaceCtx builds the emoji-image placement context, likewise.
	PlaceCtx func(send func(tea.Msg)) emojiwidth.PlaceContext
	// ProgramOptions route the program's output through the
	// frame-correlated writer. Pass them to tea.NewProgram.
	ProgramOptions []tea.ProgramOption

	emojiCells int
}

// install sets app's image and emoji contexts with send as their SendMsg.
// setupTerminalImages installs them with a nil send; call install again
// with p.Send once the tea.Program exists, before it runs, so fetches
// that complete can dispatch their ready messages into the loop.
func (t terminalImages) install(app *ui.App, send func(tea.Msg)) {
	app.SetImageContext(t.ImgCtx(send))
	app.SetEmojiContext(messages.EmojiContext{
		PlaceCtx: t.PlaceCtx(send),
		Cells:    t.emojiCells,
		Customs:  nil, // populated by CustomEmojisLoadedMsg
	})
}

// setupTerminalImages decides how app draws images in this terminal and
// wires it: the frame-correlated sixel/kitty output, protocol detection
// from the environment and cfg.Appearance.ImageProtocol, kitty placement,
// the kitty and sixel probes, emoji image mode, cell pixel metrics, and
// app's image fetcher, protocol and image/emoji contexts (with a nil
// SendMsg; see terminalImages.install). fetcher serves inline images and
// emoji.
//
// It talks to the terminal in raw mode, so it must run before the
// tea.Program starts.
func setupTerminalImages(cfg config.Config, app *ui.App, fetcher core.ImageFetcher) terminalImages {
	// Frame-correlated sixel output: the App publishes immutable
	// placement snapshots into sixelFrames; terminalOutput strips the
	// internal frame marker from Bubble Tea's window title, takes the
	// exact flushed frame, and paints its sixel operations after the
	// text diff. Kitty uploads share the same serialized writer so
	// byte streams from different goroutines can't interleave.
	sixelFrames := imgpkg.NewSixelFrameStore()
	terminalOutput := imgpkg.NewFrameOutput(os.Stdout, sixelFrames)
	imgpkg.KittyOutput = terminalOutput.SideChannel()
	app.SetSixelFrameStore(sixelFrames)

	// Detect image rendering protocol BEFORE constructing the avatar
	// cache so the cache can pick the right rendering path (kitty
	// graphics for sharp pixels, halfblock otherwise).
	termEnv := imgpkg.CaptureEnv()
	proto := imgpkg.Detect(termEnv, cfg.Appearance.ImageProtocol)
	debuglog.ImgRender("image protocol detect: cfg=%q result=%s", cfg.Appearance.ImageProtocol, proto)

	// Kitty placement: who turns placeholder cells into pixels. On a
	// terminal with kitty graphics but no unicode placeholders (WezTerm)
	// slk paints direct placements itself, which needs a cell size the
	// terminal knows: WezTerm refuses images without one, and its last
	// stable release can crash. So the cell size is measured here,
	// before the kitty probe, and kept for image encoding below. Without
	// one, fall back to the protocol such a terminal had before
	// emulation existed (sixel in auto mode).
	placement := imgpkg.KittyNative
	var pxW, pxH int // set here for emulated placement, else further down
	if proto == imgpkg.ProtoKitty {
		placement = imgpkg.ChooseKittyPlacement(termEnv, cfg.Appearance.KittyPlacement)
		if placement == imgpkg.KittyEmulated {
			var measured bool
			pxW, pxH, measured = startupCellPixels(proto)
			usable := measured && emulationAllowed(runtime.GOOS, cfg.Appearance.KittyPlacement)
			proto, placement = emulatedPlacementIfMeasured(usable, imgpkg.IsAutoProtocol(cfg.Appearance.ImageProtocol))
			switch {
			case !measured:
				debuglog.ImgRender("kitty emulated placement needs a measured cell size; using %s", proto)
			case !usable:
				debuglog.ImgRender("kitty emulated placement is not tried on %s unless set; using %s", runtime.GOOS, proto)
			}
		}
		debuglog.ImgRender("kitty placement: cfg=%q result=%s", cfg.Appearance.KittyPlacement, placement)
	}

	// Optional: run kitty version probe if detected as kitty AND stdin is a TTY.
	// Must happen BEFORE bubbletea takes over the terminal. Emulated
	// placement probes with a query: WezTerm does not answer the upload
	// ProbeKittyGraphics sends.
	if proto == imgpkg.ProtoKitty && term.IsTerminal(int(os.Stdin.Fd())) {
		state, err := term.MakeRaw(int(os.Stdin.Fd()))
		if err != nil {
			debuglog.ImgRender("kitty probe skipped: cannot enter raw mode: %v", err)
		} else {
			probe := imgpkg.ProbeKittyGraphics
			if placement == imgpkg.KittyEmulated {
				probe = imgpkg.ProbeKittyQuery
			}
			ok := probe(os.Stdout, os.Stdin, 200*time.Millisecond)
			if rerr := term.Restore(int(os.Stdin.Fd()), state); rerr != nil {
				debuglog.ImgRender("term restore after kitty probe: %v", rerr)
			}
			if !ok {
				debuglog.ImgRender("kitty probe failed, downgrading to halfblock")
				proto = imgpkg.ProtoHalfBlock
			}
		}
	}

	// Sixel capability probe (issue #116). Detect only knows the handful
	// of terminals it can name from $TERM / $TERM_PROGRAM, so plenty of
	// sixel-capable terminals (xterm -ti vt340, DomTerm, toyterm, foot
	// behind a generic TERM, …) land on halfblock and render the blocky
	// mosaic even though real pixels would work. Ask the terminal
	// directly via DA1 before settling for halfblock.
	//
	// Only in auto mode — an explicit image_protocol=halfblock must stay
	// half-block — and never inside a multiplexer: tmux keeps sixel off
	// by policy (see Detect's doc comment) and zellij has no sixel path
	// at all, so probing there only risks paying the full timeout for an
	// answer we would ignore.
	if proto == imgpkg.ProtoHalfBlock &&
		imgpkg.IsAutoProtocol(cfg.Appearance.ImageProtocol) &&
		os.Getenv("TMUX") == "" && os.Getenv("ZELLIJ") == "" &&
		term.IsTerminal(int(os.Stdin.Fd())) {
		state, err := term.MakeRaw(int(os.Stdin.Fd()))
		if err != nil {
			debuglog.ImgRender("sixel probe skipped: cannot enter raw mode: %v", err)
		} else {
			ok := imgpkg.ProbeSixel(os.Stdout, os.Stdin, 200*time.Millisecond)
			if rerr := term.Restore(int(os.Stdin.Fd()), state); rerr != nil {
				debuglog.ImgRender("term restore after sixel probe: %v", rerr)
			}
			if ok {
				debuglog.ImgRender("sixel probe succeeded, upgrading halfblock to sixel")
				proto = imgpkg.ProtoSixel
			}
		}
	}
	debuglog.ImgRender("image protocol: %s", proto)
	// Record the placement mode before anything can upload (the avatar
	// cache, the first View). A probe that downgraded kitty takes
	// emulation with it.
	if proto != imgpkg.ProtoKitty {
		placement = imgpkg.KittyNative
	}
	imgpkg.SetKittyPlacement(placement)
	debuglog.ImgRender("kitty placement: %s", placement)

	// Reconcile the emoji-as-images flag with the post-probe protocol.
	// The pre-probe block in main() (search for emojiwidth.SetImageMode)
	// turns image mode ON based on env-only detection so the emoji width
	// probe can be skipped at startup. If the interactive kitty probe
	// just above downgraded us to halfblock (zellij; tmux with
	// allow-passthrough=off), image mode must be turned back OFF or:
	//   - every emoji.Place() call misses the prerender memo (it
	//     hard-codes ProtoKitty) and falls into a fetch→ready-msg→
	//     re-render loop that pins the UI thread (see issue #50);
	//   - every emoji.Width() call runs uniseg grapheme segmentation
	//     instead of delegating to lipgloss.Width().
	// Conversely, if we landed on ProtoKitty after the probe AND the
	// user has emoji_images=on, make sure image mode is on — covers the
	// case where pre-probe detection was conservative (e.g. preCfg load
	// failed) but the real detect path succeeded.
	reconciledImageMode := proto == imgpkg.ProtoKitty && cfg.Appearance.EmojiImages == "on"
	emojiwidth.SetImageMode(reconciledImageMode, cfg.Appearance.EmojiCells)
	debuglog.ImgRender("emoji image mode: reconciled=%v (final proto=%s, emoji_images=%q)",
		reconciledImageMode, proto, cfg.Appearance.EmojiImages)

	// Cell pixel metrics for image encoding. Sixel uses them for its
	// absolute raster dimensions; kitty places by cell but uses them to
	// transmit enough source pixels for the terminal's native cell
	// resolution. See startupCellPixels for where the size comes from.
	// Emulated kitty placement measured it already, above.
	if pxW == 0 {
		pxW, pxH, _ = startupCellPixels(proto)
	}
	debuglog.ImgRender("cell pixels: %dx%d", pxW, pxH)
	imgpkg.SetCellPixels(pxW, pxH)

	// Wire the inline-image pipeline into the messages pane. SendMsg
	// stays nil here because tea.NewProgram has not run yet; the caller
	// re-installs the contexts once `p` is constructed to populate it.
	// Both installs share ImgCtx so the only difference is the SendMsg
	// callback.
	t := terminalImages{
		Proto: proto,
		ImgCtx: func(send func(tea.Msg)) imgrender.ImageContext {
			return imgrender.ImageContext{
				Protocol:    proto,
				Fetcher:     fetcher,
				KittyRender: imgpkg.KittyRendererInstance(),
				CellPixels:  image.Pt(pxW, pxH),
				MaxRows:     cfg.Appearance.MaxImageRows,
				MaxCols:     cfg.Appearance.MaxImageCols,
				SendMsg:     send,
			}
		},
		// PlaceCtx mirrors ImgCtx for emoji-image placements.
		// The Fetcher is the same instance (one cache, one prerender
		// pipeline). SendMsg dispatches EmojiImageReadyMsg through
		// bubbletea so reducers can invalidate per-surface caches.
		PlaceCtx: func(send func(tea.Msg)) emojiwidth.PlaceContext {
			return emojiwidth.PlaceContext{
				Fetcher: fetcher,
				SendMsg: func(v any) {
					if send != nil {
						if msg, ok := v.(tea.Msg); ok {
							send(msg)
						}
					}
				},
			}
		},
		// All output — every protocol — flows through the
		// frame-correlated writer: it is a serialized pass-through for
		// non-sixel frames (no marker present) and the sixel paint site
		// for marked frames.
		ProgramOptions: []tea.ProgramOption{tea.WithOutput(terminalOutput)},
		emojiCells:     cfg.Appearance.EmojiCells,
	}
	app.SetImageFetcher(fetcher)
	app.SetImageProtocol(proto)
	// Emoji-image rendering. Active only on kitty (per ImageMode
	// gate set above). When inactive the messages pane uses the
	// legacy glyph/shortcode-text rendering path.
	t.install(app, nil)
	return t
}
