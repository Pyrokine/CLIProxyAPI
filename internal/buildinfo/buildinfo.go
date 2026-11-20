// Package buildinfo exposes compile-time metadata shared across the server.
package buildinfo

// The following variables are overridden via ldflags during release builds.
// Defaults cover local development builds.
var (
	// Version is the semantic version or git describe output of the binary.
	Version = "dev"

	// Commit is the git commit SHA baked into the binary.
	Commit = "none"

	// BuildDate records when the binary was built in UTC.
	BuildDate = "unknown"

	// UpdateProtocol 标识发布物支持的更新暂停与身份协议
	UpdateProtocol = "2"

	// DataFormats 声明各数据库可读取的最小版本、最大版本和写入版本
	DataFormats = "management=1:5:5;usage=1:9:9;postgres=1:1:1"
)
