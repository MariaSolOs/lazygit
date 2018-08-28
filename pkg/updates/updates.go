package updates

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/kardianos/osext"

	getter "github.com/jesseduffield/go-getter"
	"github.com/jesseduffield/lazygit/pkg/commands"
	"github.com/jesseduffield/lazygit/pkg/config"
	"github.com/jesseduffield/lazygit/pkg/i18n"
	"github.com/sirupsen/logrus"
)

// Update checks for updates and does updates
type Updater struct {
	Log       *logrus.Entry
	Config    config.AppConfigurer
	OSCommand *commands.OSCommand
	Tr        *i18n.Localizer
}

// Updater implements the check and update methods
type Updaterer interface {
	CheckForNewUpdate()
	Update()
}

var (
	projectUrl = "https://github.com/jesseduffield/lazygit"
)

// NewUpdater creates a new updater
func NewUpdater(log *logrus.Entry, config config.AppConfigurer, osCommand *commands.OSCommand, tr *i18n.Localizer) (*Updater, error) {
	contextLogger := log.WithField("context", "updates")

	updater := &Updater{
		Log:       contextLogger,
		Config:    config,
		OSCommand: osCommand,
		Tr:        tr,
	}
	return updater, nil
}

func (u *Updater) getLatestVersionNumber() (string, error) {
	req, err := http.NewRequest("GET", projectUrl+"/releases/latest", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	byt := []byte(body)
	var dat map[string]interface{}
	if err := json.Unmarshal(byt, &dat); err != nil {
		return "", err
	}
	return dat["tag_name"].(string), nil
}

func (u *Updater) RecordLastUpdateCheck() error {
	u.Config.GetAppState().LastUpdateCheck = time.Now().Unix()
	return u.Config.SaveAppState()
}

// expecting version to be of the form `v12.34.56`
func (u *Updater) majorVersionDiffers(oldVersion, newVersion string) bool {
	if oldVersion == "unversioned" {
		return false
	}
	oldVersion = strings.TrimPrefix(oldVersion, "v")
	newVersion = strings.TrimPrefix(newVersion, "v")
	return strings.Split(oldVersion, ".")[0] != strings.Split(newVersion, ".")[0]
}

func (u *Updater) checkForNewUpdate() (string, error) {
	u.Log.Info("Checking for an updated version")
	if err := u.RecordLastUpdateCheck(); err != nil {
		return "", err
	}

	newVersion, err := u.getLatestVersionNumber()
	if err != nil {
		return "", err
	}
	u.Log.Info("Current version is " + u.Config.GetVersion())
	u.Log.Info("New version is " + newVersion)

	if newVersion == u.Config.GetVersion() {
		return "", errors.New(u.Tr.SLocalize("OnLatestVersionErr"))
	}

	if u.majorVersionDiffers(u.Config.GetVersion(), newVersion) {
		return "", errors.New(u.Tr.SLocalize("MajorVersionErr"))
	}

	rawUrl, err := u.getBinaryUrl(newVersion)
	if err != nil {
		return "", err
	}
	u.Log.Info("Checking for resource at url " + rawUrl)
	if !u.verifyResourceFound(rawUrl) {
		errMessage := u.Tr.TemplateLocalize(
			"CouldNotFindBinaryErr",
			i18n.Teml{
				"url": rawUrl,
			},
		)
		return "", errors.New(errMessage)
	}
	u.Log.Info("Verified resource is available, ready to update")

	return newVersion, nil
}

// CheckForNewUpdate checks if there is an available update
func (u *Updater) CheckForNewUpdate(onFinish func(string, error) error, userRequested bool) {
	if !userRequested && u.skipUpdateCheck() {
		return
	}

	go func() {
		newVersion, err := u.checkForNewUpdate()
		if err = onFinish(newVersion, err); err != nil {
			u.Log.Error(err)
		}
	}()
}

func (u *Updater) skipUpdateCheck() bool {
	// will remove the check for windows after adding a manifest file asking for
	// the required permissions
	if runtime.GOOS == "windows" {
		u.Log.Info("Updating is currently not supported for windows until we can fix permission issues")
		return true
	}

	if u.Config.GetVersion() == "unversioned" {
		u.Log.Info("Current version is not built from an official release so we won't check for an update")
		return true
	}

	if u.Config.GetBuildSource() != "buildBinary" {
		u.Log.Info("Binary is not built with the buildBinary flag so we won't check for an update")
		return true
	}

	userConfig := u.Config.GetUserConfig()
	if userConfig.Get("update.method") == "never" {
		u.Log.Info("Update method is set to never so we won't check for an update")
		return true
	}

	currentTimestamp := time.Now().Unix()
	lastUpdateCheck := u.Config.GetAppState().LastUpdateCheck
	days := userConfig.GetInt64("update.days")

	if (currentTimestamp-lastUpdateCheck)/(60*60*24) < days {
		u.Log.Info("Last update was too recent so we won't check for an update")
		return true
	}

	return false
}

func (u *Updater) mappedOs(os string) string {
	osMap := map[string]string{
		"darwin":  "Darwin",
		"linux":   "Linux",
		"windows": "Windows",
	}
	result, found := osMap[os]
	if found {
		return result
	}
	return os
}

func (u *Updater) mappedArch(arch string) string {
	archMap := map[string]string{
		"386":   "32-bit",
		"amd64": "x86_64",
	}
	result, found := archMap[arch]
	if found {
		return result
	}
	return arch
}

// example: https://github.com/jesseduffield/lazygit/releases/download/v0.1.73/lazygit_0.1.73_Darwin_x86_64.tar.gz
func (u *Updater) getBinaryUrl(newVersion string) (string, error) {
	extension := "tar.gz"
	if runtime.GOOS == "windows" {
		extension = "zip"
	}
	url := fmt.Sprintf(
		"%s/releases/download/%s/lazygit_%s_%s_%s.%s",
		projectUrl,
		newVersion,
		newVersion[1:],
		u.mappedOs(runtime.GOOS),
		u.mappedArch(runtime.GOARCH),
		extension,
	)
	u.Log.Info("url for latest release is " + url)
	return url, nil
}

// Update downloads the latest binary and replaces the current binary with it
func (u *Updater) Update(newVersion string, onFinish func(error) error) {
	go func() {
		err := u.update(newVersion)
		if err = onFinish(err); err != nil {
			u.Log.Error(err)
		}
	}()
}

func (u *Updater) update(newVersion string) error {
	rawUrl, err := u.getBinaryUrl(newVersion)
	if err != nil {
		return err
	}
	u.Log.Info("updating with url " + rawUrl)
	return u.downloadAndInstall(rawUrl)
}

func (u *Updater) downloadAndInstall(rawUrl string) error {
	url, err := url.Parse(rawUrl)
	if err != nil {
		return err
	}

	g := new(getter.HttpGetter)
	tempDir, err := ioutil.TempDir("", "lazygit")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tempDir)
	u.Log.Info("temp directory is " + tempDir)

	// Get it!
	if err := g.Get(tempDir, url); err != nil {
		return err
	}

	// get the path of the current binary
	binaryPath, err := osext.Executable()
	if err != nil {
		return err
	}
	u.Log.Info("binary path is " + binaryPath)

	binaryName := filepath.Base(binaryPath)
	u.Log.Info("binary name is " + binaryName)

	// Verify the main file exists
	tempPath := filepath.Join(tempDir, binaryName)
	u.Log.Info("temp path to binary is " + tempPath)
	if _, err := os.Stat(tempPath); err != nil {
		return err
	}

	// swap out the old binary for the new one
	err = os.Rename(tempPath, binaryPath)
	if err != nil {
		return err
	}
	u.Log.Info("update complete!")

	return nil
}

func (u *Updater) verifyResourceFound(rawUrl string) bool {
	resp, err := http.Head(rawUrl)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	u.Log.Info("Received status code ", resp.StatusCode)
	// 403 means the resource is there (not going to bother adding extra request headers)
	// 404 means its not
	return resp.StatusCode == 403
}
