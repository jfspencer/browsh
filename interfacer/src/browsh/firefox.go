package browsh

import (
	"bufio"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gdamore/tcell"
	"github.com/go-errors/errors"
	"github.com/spf13/viper"
)

//go:embed browsh.xpi
var browshXpi embed.FS

var (
	marionette        net.Conn
	ffCommandCount    = 0
	isQuittingFirefox = false
	marionetteTimeout = 60 * time.Second
	defaultFFPrefs    = map[string]string{
		"startup.homepage_welcome_url.additional": "''",
		"devtools.errorconsole.enabled":           "true",
		"devtools.chrome.enabled":                 "true",

		// Send Browser Console (different from Devtools console) output to
		// STDOUT.
		"browser.dom.window.dump.enabled": "true",

		// From:
		// http://hg.mozilla.org/mozilla-central/file/1dd81c324ac7/build/automation.py.in//l388
		// Make url-classifier updates so rare that they won"t affect tests.
		"urlclassifier.updateinterval": "172800",
		// Point the url-classifier to a nonexistent local URL for fast failures.
		"browser.safebrowsing.provider.0.gethashURL": "'http://localhost/safebrowsing-dummy/gethash'",
		"browser.safebrowsing.provider.0.keyURL":     "'http://localhost/safebrowsing-dummy/newkey'",
		"browser.safebrowsing.provider.0.updateURL":  "'http://localhost/safebrowsing-dummy/update'",

		// Disable self repair/SHIELD
		"browser.selfsupport.url": "'https://localhost/selfrepair'",
		// Disable Reader Mode UI tour
		"browser.reader.detectedFirstArticle": "true",

		// Set the policy firstURL to an empty string to prevent
		// the privacy info page to be opened on every "web-ext run".
		// (See #1114 for rationale)
		"datareporting.policy.firstRunURL": "''",
	}
)

func startHeadlessFirefox() {
	slog.Info("Starting Firefox in headless mode")
	checkIfFirefoxIsAlreadyRunning()
	firefoxPath := ensureFirefoxBinary()
	ensureFirefoxVersion(firefoxPath)
	// Since Firefox 128-ish, Marionette refuses to run scripts in the privileged
	// "chrome" context (which Browsh needs to set preferences) unless Firefox is
	// explicitly started with system access. Older versions just warn about the
	// unknown flag and carry on.
	args := []string{"--marionette", "--remote-allow-system-access"}
	if !viper.GetBool("firefox.with-gui") {
		args = append(args, "--headless")
	}
	profile := viper.GetString("firefox.profile")
	if profile != "browsh-default" {
		slog.Info("Using Firefox profile", "profile", profile)
		args = append(args, "-P", profile)
	} else {
		profilePath := getFirefoxProfilePath()
		slog.Info("Using default profile", "path", profilePath)
		writeFirefoxUserPrefs(profilePath)
		args = append(args, "--profile", profilePath)
	}
	firefoxProcess := exec.Command(firefoxPath, args...)
	// Firefox writes most of its diagnostics to stderr. Capture both streams so they
	// end up in the log (and in any error message) rather than scribbling over the TTY.
	output, err := firefoxProcess.StdoutPipe()
	if err != nil {
		Shutdown(err)
	}
	firefoxProcess.Stderr = firefoxProcess.Stdout
	if err := firefoxProcess.Start(); err != nil {
		Shutdown(errors.New("Failed to start Firefox (" + firefoxPath + "): " + err.Error()))
	}
	// NB: `Process` is only populated after a successful `Start()`, deferring the kill
	// any earlier dereferences a nil pointer if Firefox exits early.
	defer firefoxProcess.Process.Kill()
	var recentOutput []string
	in := bufio.NewScanner(output)
	for in.Scan() {
		line := in.Text()
		slog.Info("FF-CONSOLE", "stdout", line)
		recentOutput = append(recentOutput, line)
		if len(recentOutput) > 15 {
			recentOutput = recentOutput[1:]
		}
	}
	err = firefoxProcess.Wait()
	if isQuittingFirefox {
		slog.Info("Firefox exited as part of a normal shutdown")
		return
	}
	message := "Firefox exited unexpectedly"
	if err != nil {
		message += " (" + err.Error() + ")"
	}
	message += ".\nFirefox binary: " + firefoxPath
	if len(recentOutput) > 0 {
		message += "\nLast output from Firefox:\n  " + strings.Join(recentOutput, "\n  ")
	}
	message += "\n" + firefoxExitHint(recentOutput)
	Shutdown(errors.New(message))
}

// Try to turn well known Firefox failure modes into actionable advice
func firefoxExitHint(output []string) string {
	joined := strings.Join(output, "\n")
	if strings.Contains(joined, "already running") {
		return "Hint: Firefox thinks its profile is locked or unreadable. If no other Firefox is " +
			"running, delete the 'lock' and '.parentlock' files in the profile directory: " +
			getFirefoxProfilePath()
	}
	if strings.Contains(joined, "error while loading shared libraries") {
		return "Hint: Firefox is missing system libraries. On Debian/Ubuntu try: " +
			"sudo apt-get install --no-install-recommends libgtk-3-0 libdbus-glib-1-2 libasound2 libx11-xcb1"
	}
	if strings.Contains(joined, "requires the firefox snap") {
		return "Hint: Firefox is not actually installed, only Ubuntu's placeholder script. " +
			"Install it with 'snap install firefox' or from Mozilla's APT repository."
	}
	return "Hint: run Browsh with --debug and inspect debug.log for the full Firefox output."
}

func checkIfFirefoxIsAlreadyRunning() {
	if runtime.GOOS == "windows" {
		return
	}
	// Only look at this user's processes, otherwise another user's Firefox on a shared
	// server (or one inside a container) would stop Browsh from starting.
	processes := Shell(fmt.Sprintf("ps -u %d -o args=", os.Getuid()))
	r, _ := regexp.Compile("firefox.*--headless")
	if r.MatchString(processes) {
		Shutdown(errors.New("A headless Firefox is already running"))
	}
}

func ensureFirefoxBinary() string {
	path := viper.GetString("firefox.path")
	if path == "firefox" {
		switch runtime.GOOS {
		case "windows":
			path = getFirefoxPath()
		case "darwin":
			path = "/Applications/Firefox.app/Contents/MacOS/firefox"
		default:
			path = getFirefoxPath()
		}
	}
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			err = errors.New("Firefox binary not found: " + path)
		}
		Shutdown(err)
	}
	slog.Info("Using Firefox", "path", path)
	return path
}

// Taken from https://stackoverflow.com/a/18411978/575773
func versionOrdinal(version string) string {
	// ISO/IEC 14651:2011
	const maxByte = 1<<8 - 1
	vo := make([]byte, 0, len(version)+8)
	j := -1
	for i := 0; i < len(version); i++ {
		b := version[i]
		if '0' > b || b > '9' {
			vo = append(vo, b)
			j = -1
			continue
		}
		if j == -1 {
			vo = append(vo, 0x00)
			j = len(vo) - 1
		}
		if vo[j] == 1 && vo[j+1] == '0' {
			vo[j+1] = b
			continue
		}
		if vo[j]+1 > maxByte {
			panic("VersionOrdinal: invalid version")
		}
		vo = append(vo, b)
		vo[j]++
	}
	return string(vo)
}

// Start Firefox via the `web-ext` CLI tool. This is for development and testing,
// because I haven't been able to recreate the way `web-ext` injects an unsigned
// extension.
func startWERFirefox() {
	slog.Info("Attempting to start headless Firefox with `web-ext`")
	if IsConnectedToWebExtension {
		Shutdown(errors.New("There appears to already be an existing Web Extension connection"))
	}
	checkIfFirefoxIsAlreadyRunning()
	rootDir := Shell("git rev-parse --show-toplevel")
	args := []string{
		"run",
		"--firefox=" + rootDir + "/webext/contrib/firefoxheadless.sh",
		"--verbose",
		"--no-reload",
	}
	firefoxProcess := exec.Command(rootDir+"/webext/node_modules/.bin/web-ext", args...)
	firefoxProcess.Dir = rootDir + "/webext/dist/"
	stdout, err := firefoxProcess.StdoutPipe()
	if err != nil {
		Shutdown(err)
	}
	if err := firefoxProcess.Start(); err != nil {
		Shutdown(err)
	}
	in := bufio.NewScanner(stdout)
	for in.Scan() {
		if strings.Contains(in.Text(), "Connected to the remote Firefox debugger") {
		}
		if strings.Contains(in.Text(), "JavaScript strict") ||
			strings.Contains(in.Text(), "D-BUS") ||
			strings.Contains(in.Text(), "dbus") {
			continue
		}
		slog.Info("FF-CONSOLE", "stdout", in.Text())
	}
	slog.Info("WER Firefox unexpectedly closed")
}

// Connect to Firefox's Marionette service.
// RANT: Firefox's remote control tools are so confusing. There seem to be 2
// services that come with your Firefox binary; Marionette and the Remote
// Debugger. The latter you would expect to follow the widely supported
// Chrome standard, but no, it's merely on the roadmap. There is very little
// documentation on either. I have the impression, but I'm not sure why, that
// the Remote Debugger is better, seemingly more API methods, and as mentioned
// is on the roadmap to follow the Chrome standard.
// I've used Marionette here, simply because it was easier to reverse engineer
// from the Python Marionette package.
func firefoxMarionette() {
	var (
		err  error
		conn net.Conn
	)
	connected := false
	slog.Info("Attempting to connect to Firefox Marionette")
	start := time.Now()
	for time.Since(start) < marionetteTimeout {
		conn, err = net.Dial("tcp", "127.0.0.1:2828")
		if err != nil {
			if !strings.Contains(err.Error(), "refused") {
				Shutdown(err)
			} else {
				time.Sleep(10 * time.Millisecond)
				continue
			}
		} else {
			connected = true
			break
		}
	}
	if !connected {
		Shutdown(errors.New(fmt.Sprintf(
			"Failed to connect to Firefox's Marionette within %s", marionetteTimeout)))
	}
	marionette = conn
	go readMarionette()
	sendFirefoxCommand("WebDriver:NewSession", map[string]interface{}{})
	if viper.GetString("firefox.profile") != "browsh-default" || viper.GetBool("firefox.use-existing") {
		// Best effort only: when Browsh manages the profile the preferences are
		// written to `user.js` before launch instead, which is far more reliable
		// than running chrome-context scripts over Marionette in modern Firefox.
		setDefaultFirefoxPreferences()
	}
}

func installWebextension() {
	data, err := browshXpi.ReadFile("browsh.xpi")
	if err != nil {
		Shutdown(err)
	}
	path := path.Join(getFirefoxSharedTempDir(), "browsh-webext-addon.xpi")
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		Shutdown(err)
	}
	args := map[string]interface{}{"path": path}
	sendFirefoxCommand("Addon:Install", args)
}

// Set a Firefox preference as you would in `about:config`
// `value` needs to be supplied with quotes if it's to be used as a JS string
func setFFPreference(key string, value string) {
	var args map[string]interface{}
	var script string
	sendFirefoxCommand("Marionette:SetContext", map[string]interface{}{"value": "chrome"})
	// `Preferences.jsm` (and JSMs in general) no longer exist in modern Firefox, so
	// go straight to the `Services.prefs` global that chrome scripts have access to.
	// Preferences are set on the default branch so that anything the user has
	// explicitly set in the profile still wins.
	script = fmt.Sprintf(`
		const value = %s;
		const branch = Services.prefs.getDefaultBranch("");
		switch (typeof value) {
		case "boolean":
			branch.setBoolPref("%s", value);
			break;
		case "number":
			branch.setIntPref("%s", value);
			break;
		default:
			branch.setStringPref("%s", String(value));
		}`, value, key, key, key)
	args = map[string]interface{}{"script": script}
	sendFirefoxCommand("WebDriver:ExecuteScript", args)
	sendFirefoxCommand("Marionette:SetContext", map[string]interface{}{"value": "content"})
}

// Consume output from Marionette, we don't do anything with it. It"s just
// useful to have it in the logs.
func readMarionette() {
	buffer := make([]byte, 4096)
	count, err := marionette.Read(buffer)
	if err != nil {
		slog.Error("Error reading from Marionette connection", "error", err)
		return
	}
	slog.Info("FF-MRNT", "buffer", string(buffer[:count]))
}

func sendFirefoxCommand(command string, args map[string]interface{}) {
	slog.Info("Sending command to Firefox Marionette", "command", command, "args", args)
	fullCommand := []interface{}{0, ffCommandCount, command, args}
	marshalled, _ := json.Marshal(fullCommand)
	message := fmt.Sprintf("%d:%s", len(marshalled), marshalled)
	fmt.Fprintf(marionette, "%s", message)
	ffCommandCount++
	go readMarionette()
}

func setDefaultFirefoxPreferences() {
	for key, value := range allFirefoxPreferences() {
		setFFPreference(key, value)
	}
}

func beginTimeLimit() {
	warningLength := 10
	warningLimit := time.Duration(*timeLimit - warningLength)
	time.Sleep(warningLimit * time.Second)
	message := fmt.Sprintf("Browsh will close in %d seconds...", warningLength)
	sendMessageToWebExtension("/status," + message)
	time.Sleep(time.Duration(warningLength) * time.Second)
	quitBrowsh()
}

// Careful what you change here as it isn't tested during CI
func setupFirefox() {
	go startHeadlessFirefox()
	if *timeLimit > 0 {
		go beginTimeLimit()
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		quitBrowsh()
	}()

	firefoxMarionette()
	installWebextension()
}

func StartFirefox() {
	if !viper.GetBool("firefox.use-existing") {
		writeString(0, 16, "Waiting for Firefox to connect...", tcell.StyleDefault)
		if IsTesting {
			writeString(0, 17, "TEST MODE", tcell.StyleDefault)
			go startWERFirefox()
			firefoxMarionette()
		} else {
			setupFirefox()
		}
	} else {
		firefoxMarionette()
		writeString(0, 16, "Waiting for a user-initiated Firefox instance to connect...", tcell.StyleDefault)
	}
}

func quitFirefox() {
	isQuittingFirefox = true
	sendFirefoxCommand("Marionette:Quit", map[string]interface{}{})
}

var (
	firefoxSnapChecked = false
	firefoxSnapResult  = false
)

// Ubuntu (and derivatives) ship Firefox as a snap. Snaps are sandboxed: they can't read
// hidden directories in $HOME (eg ~/.config, where Browsh keeps its Firefox profile) and
// they have their own private /tmp (where Browsh used to write the webextension). Both
// of those make Firefox fail to start or fail to install the extension, so Browsh needs
// to know whether it's dealing with a snap.
func isSnapFirefox() bool {
	if firefoxSnapChecked {
		return firefoxSnapResult
	}
	firefoxSnapChecked = true
	if runtime.GOOS != "linux" {
		return false
	}
	firefoxPath := viper.GetString("firefox.path")
	if firefoxPath == "firefox" {
		found, err := exec.LookPath("firefox")
		if err != nil {
			return false
		}
		firefoxPath = found
	}
	resolved, err := filepath.EvalSymlinks(firefoxPath)
	if err == nil && strings.HasPrefix(resolved, "/snap/") {
		firefoxSnapResult = true
	} else if info, err := os.Stat(firefoxPath); err == nil && info.Size() <= 8192 {
		// Ubuntu's /usr/bin/firefox is a tiny shell script that execs the snap
		if content, err := os.ReadFile(firefoxPath); err == nil {
			firefoxSnapResult = strings.Contains(string(content), "/snap/bin/firefox") ||
				strings.Contains(string(content), "snap run firefox")
		}
	}
	if firefoxSnapResult {
		slog.Info("Detected snap-packaged Firefox, using snap-accessible paths")
	}
	return firefoxSnapResult
}

// A directory that both Browsh and Firefox can read and write
func getFirefoxSharedTempDir() string {
	if isSnapFirefox() {
		return getSnapFirefoxDataDir()
	}
	return os.TempDir()
}

// The snap's "common" directory survives snap refreshes and is one of the few places
// outside the snap that Firefox is allowed to read.
func getSnapFirefoxDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		Shutdown(err)
	}
	dir := filepath.Join(home, "snap", "firefox", "common", getConfigNamespace())
	if err := os.MkdirAll(dir, 0755); err != nil {
		Shutdown(err)
	}
	return dir
}

// All the preferences Browsh wants Firefox to have: the built-in defaults, then
// whatever the user configured. Values are JS literals, eg `true`, `42`, `'string'`.
func allFirefoxPreferences() map[string]string {
	prefs := map[string]string{}
	for key, value := range defaultFFPrefs {
		prefs[key] = value
	}
	for _, pref := range viper.GetStringSlice("firefox.preferences") {
		parts := strings.SplitN(pref, "=", 2)
		if len(parts) != 2 {
			slog.Warn("Ignoring malformed firefox.preferences entry", "entry", pref)
			continue
		}
		prefs[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
	}
	return prefs
}

// Convert a Browsh-style JS literal into something Firefox's `user.js` parser
// accepts: booleans and integers as-is, strings double-quoted.
func toUserPrefLiteral(value string) string {
	if value == "true" || value == "false" {
		return value
	}
	if _, err := strconv.Atoi(value); err == nil {
		return value
	}
	if len(value) >= 2 {
		first, last := value[0], value[len(value)-1]
		if (first == '\'' && last == '\'') || (first == '"' && last == '"') {
			value = value[1 : len(value)-1]
		}
	}
	return strconv.Quote(value)
}

// Firefox reads `user.js` from the profile on every startup, so this is the most
// dependable way to apply preferences to the profile that Browsh manages itself.
func writeFirefoxUserPrefs(profilePath string) {
	var builder strings.Builder
	builder.WriteString("// Generated by Browsh on every launch, do not edit.\n")
	builder.WriteString("// Use `firefox.preferences` in Browsh's config.toml instead.\n")
	prefs := allFirefoxPreferences()
	keys := make([]string, 0, len(prefs))
	for key := range prefs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Fprintf(&builder, "user_pref(%s, %s);\n", strconv.Quote(key), toUserPrefLiteral(prefs[key]))
	}
	userJS := filepath.Join(profilePath, "user.js")
	if err := os.WriteFile(userJS, []byte(builder.String()), 0644); err != nil {
		Shutdown(err)
	}
	slog.Info("Wrote Firefox preferences", "path", userJS, "count", len(keys))
}
