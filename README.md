# q2-podcasts

A podcast app for the **Shanling Q2** running [Q2 Pod](https://github.com/DiamondBond/q2-pod). Subscribe to shows by their RSS feeds, browse their episodes on the Q2 and download them over Wi-Fi. Episodes land in the card's `Podcasts` folder, where Q2 Pod resumes each one where you stopped, keeps each show's playback speed and keeps them out of your music library, play counts and scrobbles. It needs Q2 Pod 1.4.2 or later, which runs card apps.

## Install

**By hand:**

1. Download `q2-podcasts-*.zip` from the latest release.
2. Power the Q2 off, then put its microSD card in your computer.
3. Unzip the file to the card's root. The card then has `.podcasts/q2-podcasts`, `.podcasts/run`, `.podcasts/q2app` and the icons `.podcasts/icon.png` and `.podcasts/icon-40.png`. The leading dot hides the folder on macOS (Cmd+Shift+. in Finder shows it) and Linux.
4. Eject the card, put it back in the Q2 and power it on.

**Over Wi-Fi:** once Q2 Pod's [`sources.list`](https://github.com/DiamondBond/q2-pod/blob/main/sources.list) has a line for this repository (`m-d-e-v/q2-podcasts Podcasts`), open **System settings → Apps** and press **Podcasts**.

Podcasts then has a row in **Streaming**.

## Use

Open **Streaming → Podcasts**. It turns on and shows its menu:

- **New Episodes:** the newest episodes of all your shows.
- **Shows:** your subscriptions. A show lists its episodes newest first, with **Show Options** at the top.
- **Downloaded:** every episode on the Q2.
- **Downloads:** what is downloading or waiting, while anything is.
- **Sleep Timer:** 15, 30 or 45 minutes, 1 hour, or End of Episode, then playback pauses. Q2 Pod keeps the timer, so it carries on after Podcasts turns itself off. The row shows on Q2 Pod builds that have sleep timers (they list `sleep` in `/tmp/q2-addon.caps`).
- **Search Podcasts:** type a show's name to search Apple's podcast directory. You can also type a feed's address, a podcast's web page or an Apple Podcasts link. A result shows its episodes and **Subscribe**.
- **Add from Your Phone:** a QR code to scan. It opens a page on your phone where you can paste any feed's address, including private and paid feeds, search, see and download episodes, and import or export OPML.
- **Refresh All** fetches every feed again. Feeds also update when Podcasts starts and every hour while it runs.
- **Turn Off Podcasts.** Unused for 15 minutes with nothing to download, it turns itself off.

**Episodes.** Press one to download it. The row shows its progress, and pressing it again cancels. Once it's downloaded, pressing it plays it, and play continues into the show's later episodes. Hold **Play/Pause** on a downloaded episode, then hold it again, to delete it.

**Show Options:** **Update Now**, **Download New Episodes** (on: each new episode downloads by itself when the feed updates), **Download Latest Episode**, **Delete Downloads** (press twice) and **Unsubscribe** (press twice). Unsubscribing leaves downloaded episodes on the Q2.

Downloaded episodes are also in **Library → Podcasts**, which works without Wi-Fi and without Podcasts running. The Q2 stays awake while episodes download; the screen still turns off. A download cut off by Wi-Fi, standby or USB storage carries on from where it stopped.

**Moving from another podcast app:** export your subscriptions as OPML from that app. Then either import the file on the phone page, or copy it to the card's root as `podcasts.opml`; Podcasts takes it the next time it starts and renames it `podcasts.opml.imported`.

## Files

Everything on the card besides the app is kept over updates and removal:

- `.podcasts/subscriptions.opml`: your shows, in OPML any podcast app imports. Edit it by hand with Podcasts off if you like.
- `.podcasts/config.json`: the phone page's key and PIN (delete them for new ones), and `idle_minutes` (0: never turn off by itself).
- `.podcasts/feeds/`, `.podcasts/queue.json`, `.podcasts/partial/`: each feed as last fetched, the downloads waiting, and unfinished downloads.
- `Podcasts/<Show>/2024-10-07 - Episode Title.mp3`: the episodes. The date first keeps a show's folder in order.

## Limits

- **Download, then play.** Q2 Pod's card apps play from the card, so an episode plays once it has downloaded; there is no streaming.
- **Audio only.** Video episodes are left out.
- The phone page is plain HTTP on your Wi-Fi, behind a key in the QR code or the PIN on the Q2's screen.

## Build

    contrib/shanlingq2/build.sh

This needs Go 1.22 or later and python3, with no dependencies outside Go's standard library. It leaves the card files in `target/shanlingq2/.podcasts` and the release ZIP beside them. Tests:

    go test ./...
    python3 contrib/shanlingq2/test_launcher.py
    go test -tags realfeeds -run TestRealFeeds -v ./internal/library   # against real podcasts; needs the network

Pushing a tag `q2-vX.Y` that matches `cmd/q2-podcasts/main.go`'s version builds and publishes the release (`.github/workflows/release.yml`). How it fits Q2 Pod: [contrib/shanlingq2/README](contrib/shanlingq2/README).

## License

[MIT](LICENSE). `internal/q2`, `internal/q2spot`, `internal/qr`, `contrib/shanlingq2/package.py` and the launcher follow [q2-filebrowser](https://github.com/group-a-study/q2-filebrowser) (MIT); the QR encoder follows Project Nayuki's QR Code generator (MIT). The icon is Q2 Pod's own Podcasts icon (MIT). `internal/tlsroots/cacert.pem` is Mozilla's CA bundle as curl publishes it (MPL-2.0).
