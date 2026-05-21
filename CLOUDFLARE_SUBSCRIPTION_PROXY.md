# Cloudflare 订阅代理

这个方案只让 Cloudflare Worker 转发订阅文本，不转发节点流量。

客户端访问：

```text
https://do2.aigh.store/jbhd/customer-sub/订阅码
```

Worker 实际拉取：

```text
http://do.aigh.store/jbhd/customer-sub/订阅码
```

节点内容仍然由 3X-UI 生成，Hysteria2、VMess、VLESS 节点连接地址不要改成 Worker 域名。

## 部署方式

1. 在 Cloudflare 添加一个全新的干净域名，或者使用已经接入 Cloudflare 的域名。
2. 如果域名现在在 NameSilo，把 NameSilo 里的 NS 改成 Cloudflare 提供的两个 nameserver。
3. 在 Cloudflare 后台进入 `Workers & Pages`，创建一个 Worker。
4. 把 `cloudflare-worker-subscription-proxy.js` 的内容粘贴到 Worker 代码里并部署。
5. 给 Worker 绑定 Custom Domain，例如：

```text
do2.aigh.store
```

6. 访问：

```text
https://do2.aigh.store/jbhd/customer-sub/8xkz4pizp6blgbd8
```

页面应该显示一段以 `dm1lc3M6` 开头的 Base64 订阅文本。

## 可选：Wrangler 部署

安装并登录 Wrangler 后执行：

```bash
npx wrangler deploy
```

然后在 Cloudflare 后台给 Worker 绑定 Custom Domain。

## 注意事项

- 不要把节点域名改成 Worker 域名。
- 不要用 Cloudflare 普通橙云代理 Hysteria2、VMess、VLESS 节点端口。
- Worker 只适合代理订阅文本这种 HTTP 内容。
- 如果以后源站域名变了，修改 `cloudflare-worker-subscription-proxy.js` 里的 `ORIGIN_BASE`。
- 当前源站订阅入口是 `http://do.aigh.store/jbhd/customer-sub/`。
- 当前客户订阅入口建议使用 `https://do2.aigh.store/jbhd/customer-sub/`。
