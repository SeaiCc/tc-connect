package core

import "io/fs"

var webAssetsFS fs.FS

/*注册embedded 前端 assets， 由web/embed.go init()方法调用*/
func RegisterWebAssets(fsys fs.FS) {
	webAssetsFS = fsys
}
