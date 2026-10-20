// Copyright 2019 Yunion
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

package cas

// autounattend.xml for an unattended Windows install from ISO on CAS.
// Element names follow the Microsoft unattend schema (components
// Microsoft-Windows-Setup, -PnpCustomizationsWinPE,
// -PnpCustomizationsNonWinPE, -International-Core(-WinPE), -Shell-Setup).
//
// Drivers (user concern 1, "no disk found / Load driver"): the virtio
// storage and network drivers come from the floppy CAS attaches for
// storeFile=autoVirtio (api.yaml A:64828), which is A:\ in Windows PE.
// The windowsPE pass loads them before disk selection
// (PnpCustomizationsWinPE/DriverPaths, so Setup sees the virtio disk
// without anyone pressing "Load driver"), and the offlineServicing pass
// adds them to the installed image (PnpCustomizationsNonWinPE), so the
// virtio NIC works after the first boot. The per-OS folder on the floppy
// is UNVERIFIED (virtioDriverPaths, the virtio-win floppy layout); it can
// be overridden.
//
// Secrets: the administrator password is written encoded the way the
// Windows System Image Manager does it (PlainText=false: base64 of the
// UTF-16LE password followed by the element name). That is obfuscation,
// not encryption: anyone with the answer ISO can decode it, so the ISO is
// deleted after a successful run and reported otherwise. The product key
// has no encoded form in the schema and is written as is. Neither is ever
// logged; RenderRedacted produces the plan copy.

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/xml"
	"fmt"
	"io"
	"regexp"
	"strings"
	"text/template"
	"time"
	"unicode/utf16"

	"yunion.io/x/pkg/errors"

	"yunion.io/x/cloudmux/pkg/cloudprovider"
)

// SecretString never prints its value (fmt verbs, %#v, JSON).
type SecretString string

const redactedSecret = "<redacted>"

func (s SecretString) String() string   { return redactedSecret }
func (s SecretString) GoString() string { return redactedSecret }
func (s SecretString) Format(f fmt.State, verb rune) {
	io.WriteString(f, redactedSecret)
}
func (s SecretString) MarshalJSON() ([]byte, error) { return []byte(`"` + redactedSecret + `"`), nil }
func (s SecretString) MarshalText() ([]byte, error) { return []byte(redactedSecret), nil }

// Reveal is the only way to get the value back.
func (s SecretString) Reveal() string { return string(s) }

// virtioDriverPaths maps a CAS osVersion (api.yaml A:64571 list) to the
// driver folders on the autoVirtio floppy. UNVERIFIED: the layout of the
// CAS floppy is assumed to be the upstream virtio-win floppy layout
// (<arch>\<os>\ holding viostor, vioscsi and netkvm); override with
// SUnattendOptions.DriverPaths (cmx --driver-path) when it differs.
// Windows Setup also searches subfolders of each path.
var virtioDriverPaths = map[string][]string{
	"Microsoft Windows Server 2019(64-bit)":    {`A:\amd64\2k19`},
	"Microsoft Windows Server 2016(64-bit)":    {`A:\amd64\2k16`},
	"Microsoft Windows Server 2012 R2":         {`A:\amd64\2k12R2`},
	"Microsoft Windows Server 2012":            {`A:\amd64\2k12`},
	"Microsoft Windows Server 2008 R2(64-bit)": {`A:\amd64\2k8R2`},
	"Microsoft Windows Server 2008(64-bit)":    {`A:\amd64\2k8`},
	"Microsoft Windows Server 2008(32-bit)":    {`A:\i386\2k8`},
	"Microsoft Windows 10(64-bit)":             {`A:\amd64\Win10`},
	"Microsoft Windows 10(32-bit)":             {`A:\i386\Win10`},
	"Microsoft Windows 8.1(64-bit)":            {`A:\amd64\Win8.1`},
	"Microsoft Windows 8.1(32-bit)":            {`A:\i386\Win8.1`},
	"Microsoft Windows 8(64-bit)":              {`A:\amd64\Win8`},
	"Microsoft Windows 8(32-bit)":              {`A:\i386\Win8`},
	"Microsoft Windows 7(64-bit)":              {`A:\amd64\Win7`},
	"Microsoft Windows 7(32-bit)":              {`A:\i386\Win7`},
}

// windowsArch is the unattend processorArchitecture for an osVersion.
func windowsArch(osVersion string) string {
	if strings.Contains(osVersion, "32-bit") {
		return "x86"
	}
	return "amd64"
}

var (
	unattendLocaleRe   = regexp.MustCompile(`^[A-Za-z]{2,3}(-[A-Za-z0-9]{2,8})*$`)
	unattendInputRe    = regexp.MustCompile(`^([A-Za-z]{2,3}(-[A-Za-z0-9]{2,8})*|[0-9a-fA-F]{4}:[0-9a-fA-F]{8})(;([A-Za-z]{2,3}(-[A-Za-z0-9]{2,8})*|[0-9a-fA-F]{4}:[0-9a-fA-F]{8}))*$`)
	unattendDriverRe   = regexp.MustCompile(`^[A-Za-z]:\\[^<>"|?*\r\n]*$`)
	unattendProductKey = regexp.MustCompile(`^[A-Z0-9]{5}(-[A-Z0-9]{5}){4}$`)
)

// SUnattendOptions are the parameters of autounattend.xml.
type SUnattendOptions struct {
	// OsVersion is the CAS osVersion; it picks the architecture and the
	// default driver paths.
	OsVersion string
	// ImageIndex (1-based, /IMAGE/INDEX) or ImageName (/IMAGE/NAME, e.g.
	// "Windows Server 2019 SERVERSTANDARD") selects the edition in
	// install.wim. One is required.
	ImageIndex int
	ImageName  string
	// ProductKey is optional (evaluation and KMS media need none).
	ProductKey SecretString
	// AdminPassword is the built-in Administrator password (required).
	AdminPassword SecretString
	// Locale is UILanguage/SystemLocale/UserLocale (default en-US);
	// InputLocale defaults to Locale.
	Locale      string
	InputLocale string
	// TimeZone is a Windows time zone id (default "China Standard Time").
	TimeZone string
	// DriverPaths overrides virtioDriverPaths[OsVersion].
	DriverPaths []string
	// DiskLayout "mbr" (BIOS, default) or "gpt" (UEFI). UNVERIFIED which
	// firmware CAS gives a new VM; mbr matches a BIOS (SeaBIOS) guest.
	DiskLayout string
	// CastoolsInstaller, when set, is run from the CAStools CD in
	// FirstLogonCommands: the root of every CD drive is searched for it
	// (CAS_tools_setup.exe per the H3C docs). CastoolsArgs are its
	// silent-install switches. UNVERIFIED: the silent switch; without one
	// the wizard blocks the logon commands until the install timeout.
	CastoolsInstaller string
	CastoolsArgs      string
	// ComputerName defaults to "*" (random; Sysprep generalizes anyway).
	ComputerName string
}

const unattendWinlogonKey = `"HKLM\SOFTWARE\Microsoft\Windows NT\CurrentVersion\Winlogon"`

// unattendCleanupCommands run after CAStools and right before Sysprep.
// A failing command (e.g. a value that does not exist) does not stop the
// following ones.
var unattendCleanupCommands = []unattendCommand{
	{Line: `cmd /c del /f /q C:\Windows\Panther\unattend*.xml C:\Windows\Panther\autounattend*.xml C:\Windows\Panther\Unattend\*.xml C:\Windows\System32\Sysprep\unattend.xml C:\Windows\System32\Sysprep\autounattend.xml`, Description: "Delete cached answer files (they hold the encoded password)"},
	{Line: `reg add ` + unattendWinlogonKey + ` /v AutoAdminLogon /t REG_SZ /d 0 /f`, Description: "Disable autologon"},
	{Line: `reg delete ` + unattendWinlogonKey + ` /v DefaultPassword /f`, Description: "Clear the autologon password"},
	{Line: `reg delete ` + unattendWinlogonKey + ` /v AutoLogonCount /f`, Description: "Clear the autologon count"},
}

const (
	DEFAULT_UNATTEND_LOCALE   = "en-US"
	DEFAULT_UNATTEND_TIMEZONE = "China Standard Time"
	UNATTEND_SYSPREP_COMMAND  = `C:\Windows\System32\Sysprep\sysprep.exe /generalize /oobe /shutdown /quiet`
)

func (o *SUnattendOptions) normalize() error {
	if !strings.HasPrefix(o.OsVersion, "Microsoft Windows") {
		return errors.Wrapf(cloudprovider.ErrInputParameter, "osVersion %q is not a CAS Windows version (e.g. \"Microsoft Windows Server 2019(64-bit)\")", o.OsVersion)
	}
	if (o.ImageIndex > 0) == (len(o.ImageName) > 0) {
		return errors.Wrap(cloudprovider.ErrInputParameter, "give exactly one of image index (>= 1) or image name")
	}
	if len(o.AdminPassword) == 0 {
		return errors.Wrap(cloudprovider.ErrMissingParameter, "administrator password")
	}
	if strings.ContainsAny(o.AdminPassword.Reveal(), "\x00\r\n") {
		return errors.Wrap(cloudprovider.ErrInputParameter, "administrator password contains a control character")
	}
	if pk := o.ProductKey.Reveal(); len(pk) > 0 && !unattendProductKey.MatchString(pk) {
		return errors.Wrap(cloudprovider.ErrInputParameter, "product key must look like XXXXX-XXXXX-XXXXX-XXXXX-XXXXX")
	}
	if len(o.Locale) == 0 {
		o.Locale = DEFAULT_UNATTEND_LOCALE
	}
	if len(o.InputLocale) == 0 {
		o.InputLocale = o.Locale
	}
	if !unattendLocaleRe.MatchString(o.Locale) || !unattendInputRe.MatchString(o.InputLocale) {
		return errors.Wrapf(cloudprovider.ErrInputParameter, "locale %q / input locale %q", o.Locale, o.InputLocale)
	}
	if len(o.TimeZone) == 0 {
		o.TimeZone = DEFAULT_UNATTEND_TIMEZONE
	}
	if strings.ContainsAny(o.TimeZone, "<>&\r\n") {
		return errors.Wrapf(cloudprovider.ErrInputParameter, "time zone %q", o.TimeZone)
	}
	if len(o.DriverPaths) == 0 {
		o.DriverPaths = virtioDriverPaths[o.OsVersion]
	}
	if len(o.DriverPaths) == 0 {
		return errors.Wrapf(cloudprovider.ErrInputParameter, "no default virtio driver path for %q; give one with --driver-path (e.g. A:\\amd64\\2k19)", o.OsVersion)
	}
	for _, p := range o.DriverPaths {
		if !unattendDriverRe.MatchString(p) {
			return errors.Wrapf(cloudprovider.ErrInputParameter, "driver path %q (drive:\\folder)", p)
		}
	}
	switch o.DiskLayout {
	case "":
		o.DiskLayout = "mbr"
	case "mbr", "gpt":
	default:
		return errors.Wrapf(cloudprovider.ErrInputParameter, "disk layout %q (mbr or gpt)", o.DiskLayout)
	}
	if strings.ContainsAny(o.CastoolsInstaller, "\"<>&|\r\n%") || strings.ContainsAny(o.CastoolsArgs, "\"<>&|\r\n%") {
		return errors.Wrap(cloudprovider.ErrInputParameter, "CAStools installer/args contain a shell or XML metacharacter")
	}
	if len(o.ComputerName) == 0 {
		o.ComputerName = "*"
	}
	return nil
}

// encodeUnattendPassword is the PlainText=false form: base64(UTF-16LE of
// password + element name), as Windows System Image Manager writes it.
func encodeUnattendPassword(password, element string) string {
	u := utf16.Encode([]rune(password + element))
	b := make([]byte, 2*len(u))
	for i, c := range u {
		binary.LittleEndian.PutUint16(b[2*i:], c)
	}
	return base64.StdEncoding.EncodeToString(b)
}

type unattendCommand struct {
	Order       int
	Line        string
	Description string
}

type unattendData struct {
	*SUnattendOptions
	Arch          string
	ImageKey      string
	ImageValue    string
	AdminPassword string // encoded or redacted
	AutoLogonPw   string // encoded or redacted
	ProductKey    string // plain or redacted
	Commands      []unattendCommand
}

func xmlEscape(s string) string {
	b := bytes.Buffer{}
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

var unattendTemplate = template.Must(template.New("unattend").Funcs(template.FuncMap{"x": xmlEscape, "inc": func(i int) int { return i + 1 }}).Parse(`<?xml version="1.0" encoding="utf-8"?>
<!-- generated by cloudmux (CAS Windows ISO import); contains secrets: delete after use -->
<unattend xmlns="urn:schemas-microsoft-com:unattend" xmlns:wcm="http://schemas.microsoft.com/WMIConfig/2002/State">
  <settings pass="windowsPE">
    <component name="Microsoft-Windows-International-Core-WinPE" processorArchitecture="{{.Arch}}" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS">
      <SetupUILanguage>
        <UILanguage>{{x .Locale}}</UILanguage>
      </SetupUILanguage>
      <InputLocale>{{x .InputLocale}}</InputLocale>
      <SystemLocale>{{x .Locale}}</SystemLocale>
      <UILanguage>{{x .Locale}}</UILanguage>
      <UserLocale>{{x .Locale}}</UserLocale>
    </component>
    <component name="Microsoft-Windows-PnpCustomizationsWinPE" processorArchitecture="{{.Arch}}" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS">
      <DriverPaths>
{{- range $i, $p := .DriverPaths}}
        <PathAndCredentials wcm:action="add" wcm:keyValue="{{inc $i}}">
          <Path>{{x $p}}</Path>
        </PathAndCredentials>
{{- end}}
      </DriverPaths>
    </component>
    <component name="Microsoft-Windows-Setup" processorArchitecture="{{.Arch}}" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS">
      <DiskConfiguration>
        <!-- disk 0 is the new, empty system disk of the temporary VM (its only disk) -->
        <Disk wcm:action="add">
          <DiskID>0</DiskID>
          <WillWipeDisk>true</WillWipeDisk>
          <CreatePartitions>
{{- if eq .DiskLayout "gpt"}}
            <CreatePartition wcm:action="add"><Order>1</Order><Type>EFI</Type><Size>260</Size></CreatePartition>
            <CreatePartition wcm:action="add"><Order>2</Order><Type>MSR</Type><Size>16</Size></CreatePartition>
            <CreatePartition wcm:action="add"><Order>3</Order><Type>Primary</Type><Extend>true</Extend></CreatePartition>
{{- else}}
            <CreatePartition wcm:action="add"><Order>1</Order><Type>Primary</Type><Extend>true</Extend></CreatePartition>
{{- end}}
          </CreatePartitions>
          <ModifyPartitions>
{{- if eq .DiskLayout "gpt"}}
            <ModifyPartition wcm:action="add"><Order>1</Order><PartitionID>1</PartitionID><Format>FAT32</Format><Label>System</Label></ModifyPartition>
            <ModifyPartition wcm:action="add"><Order>2</Order><PartitionID>3</PartitionID><Format>NTFS</Format><Label>Windows</Label><Letter>C</Letter></ModifyPartition>
{{- else}}
            <ModifyPartition wcm:action="add"><Order>1</Order><PartitionID>1</PartitionID><Format>NTFS</Format><Label>Windows</Label><Letter>C</Letter><Active>true</Active></ModifyPartition>
{{- end}}
          </ModifyPartitions>
        </Disk>
        <WillShowUI>OnError</WillShowUI>
      </DiskConfiguration>
      <ImageInstall>
        <OSImage>
          <InstallFrom>
            <MetaData wcm:action="add">
              <Key>{{.ImageKey}}</Key>
              <Value>{{x .ImageValue}}</Value>
            </MetaData>
          </InstallFrom>
          <InstallTo>
            <DiskID>0</DiskID>
            <PartitionID>{{if eq .DiskLayout "gpt"}}3{{else}}1{{end}}</PartitionID>
          </InstallTo>
          <WillShowUI>OnError</WillShowUI>
        </OSImage>
      </ImageInstall>
      <UserData>
        <AcceptEula>true</AcceptEula>
{{- if .ProductKey}}
        <ProductKey>
          <Key>{{x .ProductKey}}</Key>
          <WillShowUI>OnError</WillShowUI>
        </ProductKey>
{{- end}}
      </UserData>
    </component>
  </settings>
  <settings pass="offlineServicing">
    <component name="Microsoft-Windows-PnpCustomizationsNonWinPE" processorArchitecture="{{.Arch}}" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS">
      <DriverPaths>
{{- range $i, $p := .DriverPaths}}
        <PathAndCredentials wcm:action="add" wcm:keyValue="{{inc $i}}">
          <Path>{{x $p}}</Path>
        </PathAndCredentials>
{{- end}}
      </DriverPaths>
    </component>
  </settings>
  <settings pass="specialize">
    <component name="Microsoft-Windows-Shell-Setup" processorArchitecture="{{.Arch}}" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS">
      <ComputerName>{{x .ComputerName}}</ComputerName>
      <TimeZone>{{x .TimeZone}}</TimeZone>
    </component>
  </settings>
  <settings pass="oobeSystem">
    <component name="Microsoft-Windows-International-Core" processorArchitecture="{{.Arch}}" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS">
      <InputLocale>{{x .InputLocale}}</InputLocale>
      <SystemLocale>{{x .Locale}}</SystemLocale>
      <UILanguage>{{x .Locale}}</UILanguage>
      <UserLocale>{{x .Locale}}</UserLocale>
    </component>
    <component name="Microsoft-Windows-Shell-Setup" processorArchitecture="{{.Arch}}" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS">
      <OOBE>
        <HideEULAPage>true</HideEULAPage>
        <HideOnlineAccountScreens>true</HideOnlineAccountScreens>
        <HideWirelessSetupInOOBE>true</HideWirelessSetupInOOBE>
        <ProtectYourPC>3</ProtectYourPC>
      </OOBE>
      <UserAccounts>
        <AdministratorPassword>
          <Value>{{x .AdminPassword}}</Value>
          <PlainText>false</PlainText>
        </AdministratorPassword>
      </UserAccounts>
      <AutoLogon>
        <Enabled>true</Enabled>
        <Username>Administrator</Username>
        <LogonCount>1</LogonCount>
        <Password>
          <Value>{{x .AutoLogonPw}}</Value>
          <PlainText>false</PlainText>
        </Password>
      </AutoLogon>
      <FirstLogonCommands>
{{- range .Commands}}
        <SynchronousCommand wcm:action="add">
          <Order>{{.Order}}</Order>
          <CommandLine>{{x .Line}}</CommandLine>
          <Description>{{x .Description}}</Description>
        </SynchronousCommand>
{{- end}}
      </FirstLogonCommands>
      <TimeZone>{{x .TimeZone}}</TimeZone>
    </component>
  </settings>
</unattend>
`))

func (o *SUnattendOptions) data(redact bool) *unattendData {
	d := &unattendData{SUnattendOptions: o, Arch: windowsArch(o.OsVersion)}
	if o.ImageIndex > 0 {
		d.ImageKey, d.ImageValue = "/IMAGE/INDEX", fmt.Sprintf("%d", o.ImageIndex)
	} else {
		d.ImageKey, d.ImageValue = "/IMAGE/NAME", o.ImageName
	}
	if redact {
		d.AdminPassword, d.AutoLogonPw = redactedSecret, redactedSecret
		if len(o.ProductKey) > 0 {
			d.ProductKey = redactedSecret
		}
	} else {
		d.AdminPassword = encodeUnattendPassword(o.AdminPassword.Reveal(), "AdministratorPassword")
		d.AutoLogonPw = encodeUnattendPassword(o.AdminPassword.Reveal(), "Password")
		d.ProductKey = o.ProductKey.Reveal()
	}
	order := 1
	if len(o.CastoolsInstaller) > 0 {
		inst := strings.TrimLeft(o.CastoolsInstaller, `\`)
		line := fmt.Sprintf(`cmd /c for %%d in (D E F G H I J K) do if exist %%d:\%s start "" /wait %%d:\%s %s`, inst, inst, o.CastoolsArgs)
		d.Commands = append(d.Commands, unattendCommand{Order: order, Line: strings.TrimSpace(line), Description: "Install CAStools from the CAStools CD if mounted"})
		order++
	}
	// N-W4: before Sysprep, remove the cached answer files (they carry the
	// encoded password and would land in the template) and turn
	// autologon off. UNVERIFIED (lab L-W5): that nothing else keeps a copy
	// (Setup keeps the AutoLogon password as an LSA secret, cleared by
	// Windows once LogonCount is used up).
	for _, c := range unattendCleanupCommands {
		d.Commands = append(d.Commands, unattendCommand{Order: order, Line: c.Line, Description: c.Description})
		order++
	}
	d.Commands = append(d.Commands, unattendCommand{Order: order, Line: UNATTEND_SYSPREP_COMMAND, Description: "Generalize and shut down (CAS sees the VM shut off)"})
	return d
}

func (o *SUnattendOptions) render(redact bool) ([]byte, error) {
	if err := o.normalize(); err != nil {
		return nil, err
	}
	b := bytes.Buffer{}
	if err := unattendTemplate.Execute(&b, o.data(redact)); err != nil {
		return nil, errors.Wrapf(err, "render autounattend.xml")
	}
	return b.Bytes(), nil
}

// Render returns autounattend.xml with the real (encoded) secrets, for
// the answer ISO only.
func (o *SUnattendOptions) Render() ([]byte, error) { return o.render(false) }

// RenderRedacted returns the same document with every secret replaced by
// <redacted>, for plans and logs.
func (o *SUnattendOptions) RenderRedacted() ([]byte, error) { return o.render(true) }

// ANSWER_ISO_LABEL is the volume id of the generated answer ISO.
const ANSWER_ISO_LABEL = "CAS_UNATTEND"

// DEFAULT_CASTOOLS_INSTALLER is the Windows installer at the root of the
// CAStools ISO per the H3C CAS/UIS docs ("CAS_tools_setup.exe").
const DEFAULT_CASTOOLS_INSTALLER = "CAS_tools_setup.exe"

// BuildAnswerISO renders autounattend.xml and packs it as the only file
// of an ISO 9660 + Joliet image. The bytes hold the encoded secrets.
func (o *SUnattendOptions) BuildAnswerISO(t time.Time) ([]byte, error) {
	xmlData, err := o.Render()
	if err != nil {
		return nil, err
	}
	return BuildISO(ANSWER_ISO_LABEL, []SISOFile{{Name: "autounattend.xml", Data: xmlData}}, t)
}
