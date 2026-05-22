```
# 安装packages.json依赖到web/node_modules 中
cd web && npm install
# 执行web/package.json中 scripts.build的构建命令
npm run build


go build -tags 'no_acp no_line' -ldflags -s -w -X main.version=dev -X main.commit=none -X main.buildTime=2026-04-29T09:07:33Z -o c-connect ./cmd/tc-connect
```