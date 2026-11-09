// Package web 把前端静态文件打进二进制。
//
// 用 go:embed 而不是把 dist 目录挂进容器，理由很实际：dlog 要部署到
// 十几台机器上，单文件 scp 过去就能跑，不用再操心前端文件丢没丢。
package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static
var content embed.FS

// Handler 返回静态资源处理器。
func Handler() (http.Handler, error) {
	sub, err := fs.Sub(content, "static")
	if err != nil {
		return nil, err
	}
	return http.FileServer(http.FS(sub)), nil
}

// LoginHTML 返回登录页，供未授权请求直接输出。
// 它走不了 FileServer（那条路径本身就要求鉴权），所以单独读一份。
func LoginHTML() []byte {
	data, err := content.ReadFile("static/login.html")
	if err != nil {
		return []byte(`<!doctype html><meta charset="utf-8"><p style="font-family:sans-serif;padding:24px">登录页缺失，请重新构建。</p>`)
	}
	return data
}
