/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package flogging

import (
	rotatelogs "github.com/lestrrat/go-file-rotatelogs"
	"go.uber.org/zap/zapcore"
	"os"
	"os/exec"
	"path"
	"time"
)

type Encoding int8

const (
	CONSOLE = iota
	JSON
	LOGFMT
)

// EncodingSelector is used to determine whether log records are
// encoded as JSON or in human readable CONSOLE or LOGFMT formats.
type EncodingSelector interface {
	Encoding() Encoding
}

// Core is a custom implementation of a zapcore.Core. It's a terrible hack that
// only exists to work around the intersection of state associated with
// encoders, implementation hiding in zapcore, and implicit, ad-hoc logger
// initialization within fabric.
//
// In addition to encoding log entries and fields to a buffer, zap Encoder
// implementations also need to maintain field state. When zapcore.Core.With is
// used, the associated encoder is cloned and the fields are added to the
// encoder. This means that encoder instances cannot be shared across cores.
//
// In terms of implementation hiding, it's difficult for our FormatEncoder to
// cleanly wrap the JSON and console implementations from zap as all methods
// from the zapcore.ObjectEncoder would need to be implemented to delegate to
// the correct backend.
//
// This implementation works by associating multiple encoders with a core. When
// fields are added to the core, the fields are added to all of the encoder
// implementations. The core also references the logging configuration to
// determine the proper encoding to use, the writer to delegate to, and the
// enabled levels.
type Core struct {
	zapcore.LevelEnabler
	Levels   *LoggerLevels
	Encoders map[Encoding]zapcore.Encoder
	Selector EncodingSelector
	Output   zapcore.WriteSyncer
	Observer Observer
}

//go:generate counterfeiter -o mock/observer.go -fake-name Observer . Observer

type Observer interface {
	Check(e zapcore.Entry, ce *zapcore.CheckedEntry)
	WriteEntry(e zapcore.Entry, fields []zapcore.Field)
}

func (c *Core) With(fields []zapcore.Field) zapcore.Core {
	clones := map[Encoding]zapcore.Encoder{}
	for name, enc := range c.Encoders {
		clone := enc.Clone()
		addFields(clone, fields)
		clones[name] = clone
	}

	return &Core{
		LevelEnabler: c.LevelEnabler,
		Levels:       c.Levels,
		Encoders:     clones,
		Selector:     c.Selector,
		Output:       c.Output,
		Observer:     c.Observer,
	}
}

func (c *Core) Check(e zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.Observer != nil {
		c.Observer.Check(e, ce)
	}

	if c.Enabled(e.Level) && c.Levels.Level(e.LoggerName).Enabled(e.Level) {
		return ce.AddCore(e, c)
	}
	return ce
}

func (c *Core) Write(e zapcore.Entry, fields []zapcore.Field) error {
	encoding := c.Selector.Encoding()
	enc := c.Encoders[encoding]

	buf, err := enc.EncodeEntry(e, fields)
	if err != nil {
		return err
	}
	_, err = c.Output.Write(buf.Bytes())
	buf.Free()
	if err != nil {
		return err
	}

	if e.Level >= zapcore.PanicLevel {
		c.Sync()
	}

	if c.Observer != nil {
		c.Observer.WriteEntry(e, fields)
	}

	return nil
}

func (c *Core) Sync() error {
	return c.Output.Sync()
}

func addFields(enc zapcore.ObjectEncoder, fields []zapcore.Field) {
	for i := range fields {
		fields[i].AddTo(enc)
	}
}

func NewWriter(name, appname, suffix string) *rotatelogs.RotateLogs {
	// 归档日志路径
	backupPath := path.Join(name, "backup")
	if _, err := os.Stat(backupPath); err != nil {
		exec.Command("mkdir", "-p", backupPath).Output()
	}

	writer, err := rotatelogs.New(
		path.Join(name, "backup", appname+suffix)+".%Y%m%d.%H",
		// WithLinkName为最新的日志建立软连接，以方便随着找到当前日志文件
		rotatelogs.WithLinkName(path.Join(name, appname+suffix)),

		// WithRotationTime设置日志分割的时间，这里设置为一天分割一次
		rotatelogs.WithRotationTime(time.Hour*24),

		// WithMaxAge设置文件清理前的最长保存时间，
		rotatelogs.WithMaxAge(time.Hour*24*30),
	)
	if err != nil {
		return nil
	}
	return writer
}

func GoPodname() string {
	return os.Getenv("POD_NAME")
}

func GoNamespace() string {
	return os.Getenv("NAMESPACE")
}

func GoDeployment() string {
	return os.Getenv("DEPLOYMENT_NAME")
}

func GoNodeType() string {
	if os.Getenv("CORE_PEER_ID") != "" {
		return "peer"
	}
	return "orderer"
}
