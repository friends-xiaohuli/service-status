# Vercel Static Deploy

上传此目录到 Vercel 后即可部署。

部署前请在 Vercel 项目中设置：

- `Root Directory` = `release/vercel-static-upload`
- 环境变量 `STATUS_API_URL`

本地测试建议：

- 在本机先启动 Go 后端，默认地址通常为 `http://127.0.0.1:8080/api/status`
- 在 Vercel 本地开发或本地模拟时，将 `STATUS_API_URL` 设为 `http://127.0.0.1:8080/api/status`
- 如果直接双击打开 `index.html`，前端会默认请求 `http://127.0.0.1:8080/api/status`
- 也可以通过地址参数覆盖，例如：`index.html?api=http://127.0.0.1:8080/api/status`

线上部署建议：

- `STATUS_API_URL` 改为实际可访问的 Go 后端地址，例如 `http://101.43.43.100:8080/api/status`

部署完成后，前端只会请求当前站点下的 `/api/status`，不会在浏览器里直接暴露源 IP 和端口。

当前版本会：

- 先读取浏览器本地缓存的 summary 和 history，再校验远端时间戳。
- 通过 `view=summary` 获取紧凑字段的概要数据。
- 通过 `view=history&keys=...` 按 `service key` 分批懒加载历史数据。
- 在代理层和 Go 后端两侧都尽量启用 gzip / 条件请求，减少传输体积。
