// Copyright 2017 fatedier, fatedier@gmail.com
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

package vhost

import (
	"bytes"
	"io"
	"net/http"
	"os"

	frpLog "github.com/fatedier/frp/pkg/util/log"
	"github.com/fatedier/frp/pkg/util/version"
)

var NotFoundPagePath = ""

const (
	NotFound = `<!DOCTYPE html>
	<html lang="zh">
	<head>
		<meta charset="UTF-8">
		<title>ChmlFrp-无法找到您所请求的网站</title>
		<link rel="stylesheet" href="https://chmlfrp.cn/frp/static/css/style.css">
		<link rel="shortcut icon" href="https://chmlfrp.cn/favicon.ico" type="image/x-icon">
		<style>
			body {
				margin: 0;
				overflow: hidden;
			}
	
			#canvasContainer {
				position: absolute;
				width: 100%;
				height: 100%;
				z-index: -1;
			}
	
			canvas {
				width: 100%;
				height: 100%;
			}
		</style>
	</head>
	<body class="grid" style="overflow:hidden;">
		<main class="grid" style="--n: 3; --k: 0">
			<article class="grid" id="a0" style="--i: 0">
				<h3 class="c--ini fade">ChmlFrp-错误警告</h3>
				<p class="c--ini fade">无法找到您所请求的网站,请确定ChmlFrp服务已正常启动并检查内网端口是否正确。Unable to find the website you requested.
					Please ensure that the ChmlFrp service has started properly</p><a class="nav c--ini fade"
					href="#a1">Next</a>
				<section class="grid c--fin" role="img" aria-label="错误页面随机图片"
					style="--img: url(https://uapis.cn/api/imgapi/bq/youshou.php); --m: 8">
					<div class="slice" aria-hidden="true" style="--j: 0"></div>
					<div class="slice" aria-hidden="true" style="--j: 1"></div>
					<div class="slice" aria-hidden="true" style="--j: 2"></div>
					<div class="slice" aria-hidden="true" style="--j: 3"></div>
					<div class="slice" aria-hidden="true" style="--j: 4"></div>
					<div class="slice" aria-hidden="true" style="--j: 5"></div>
					<div class="slice" aria-hidden="true" style="--j: 6"></div>
					<div class="slice" aria-hidden="true" style="--j: 7"></div>
				</section><a class="det grid c--fin fade" href="https://chmlfrp.cn">访问ChmlFrp</a>
			</article>
			<article class="grid" id="a1" style="--i: 1">
				<h3 class="c--ini fade">可能的原因</h3>
				<p class="c--ini fade">1：您尚未启动映射。2：错误的本地端口。3：错误的外网端口。4：映射协议错误。5：您域名解析尚未生效或尚未更改</p><a
					class="nav c--ini fade" href="https://chmlfrp.cn">Next</a>
				<section class="grid c--fin" role="img" aria-label="错误页面随机图片"
					style="--img: url(https://uapis.cn/api/imgapi/bq/maomao.php); --m: 8">
					<div class="slice" aria-hidden="true" style="--j: 0"></div>
					<div class="slice" aria-hidden="true" style="--j: 1"></div>
					<div class="slice" aria-hidden="true" style="--j: 2"></div>
					<div class="slice" aria-hidden="true" style="--j: 3"></div>
					<div class="slice" aria-hidden="true" style="--j: 4"></div>
					<div class="slice" aria-hidden="true" style="--j: 5"></div>
					<div class="slice" aria-hidden="true" style="--j: 6"></div>
					<div class="slice" aria-hidden="true" style="--j: 7"></div>
				</section><a class="det grid c--fin fade" href="https://chmlfrp.cn">访问ChmlFrp</a>
			</article>
		</main>
	</body>
	<script src="https://chmlfrp.cn/frp/static/js/script.js"></script>
	<script>
		var canvas = document.getElementById("myCanvas");
		var ctx = canvas.getContext("2d");
	</script>
	</html>
`
)

func getNotFoundPageContent() []byte {
	var (
		buf []byte
		err error
	)
	if NotFoundPagePath != "" {
		buf, err = os.ReadFile(NotFoundPagePath)
		if err != nil {
			frpLog.Warn("read custom 404 page error: %v", err)
			buf = []byte(NotFound)
		}
	} else {
		buf = []byte(NotFound)
	}
	return buf
}

func notFoundResponse() *http.Response {
	header := make(http.Header)
	header.Set("server", "frp/"+version.Full())
	header.Set("Content-Type", "text/html")

	content := getNotFoundPageContent()
	res := &http.Response{
		Status:        "Not Found",
		StatusCode:    404,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        header,
		Body:          io.NopCloser(bytes.NewReader(content)),
		ContentLength: int64(len(content)),
	}
	return res
}
