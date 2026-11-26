// Package version 提供编译期版本号，由 scripts/bump-version.sh 维护。
package version

// Version 是当前版本号（semver）。发布构建会用
// -ldflags "-X jobworkspace/internal/version.Version=vX.Y.Z" 覆盖。
var Version = "1.1.1"
