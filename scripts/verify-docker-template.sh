#!/bin/bash
# M6 真机验证：用本地运行时镜像跑一个 docker 模板容器
# 验证「通用 docker 模板」的容器行为（M6 / F9）
#
# 为什么单独留一个脚本：模板能不能用，取决于这些参数**真的生效** ——
# 只读根、能力全丢弃、no-new-privileges、卷挂载、环境变量、端口、日志、能停。
# 单测只能验证 argv 拼得对，生效与否必须在真机上验。
#
# 用法：在装了 docker 的节点上 bash scripts/verify-docker-template.sh
# 镜像可用 ATL_IMG 覆盖（默认用节点上的运行时镜像，无需外网）。
set -u
NET=atl-e2e-net
NAME=atl-e2e-docker
DIR=/tmp/e2e-inst
IMG="${ATL_IMG:-atl-mcpanel-runtime:latest}"

docker rm -f $NAME >/dev/null 2>&1
docker network rm $NET >/dev/null 2>&1
rm -rf $DIR; mkdir -p $DIR/www
echo hello-mcpanel > $DIR/www/index.html

echo '=== 1. 按 daemon 的实际参数拼装 docker run ==='
docker network create $NET >/dev/null
docker run -d --rm -i --name $NAME \
  --user 1000:1000 \
  --pids-limit 256 --cap-drop ALL --security-opt no-new-privileges \
  --read-only --tmpfs /tmp:size=64m \
  -v $DIR:/data -w /data \
  --publish 127.0.0.1:28081:80 \
  -v $DIR/www:/usr/share/nginx/html:ro \
  -e FOO=bar \
  --network $NET \
  $IMG /bin/bash -c 'while true; do echo "tick $(date +%s)"; sleep 3; done' >/dev/null
echo "退出码=$?"

sleep 3
echo '=== 2. 容器状态 ==='
docker ps --filter name=$NAME --format '{{.Names}}  {{.Status}}  {{.Image}}'

echo '=== 3. 隔离参数是否生效（应看到 read-only / cap_drop / no-new-privileges）==='
docker inspect $NAME --format 'ReadonlyRootfs={{.HostConfig.ReadonlyRootfs}} CapDrop={{.HostConfig.CapDrop}} SecurityOpt={{.HostConfig.SecurityOpt}} User={{.Config.User}}'

echo '=== 4. 卷挂载是否生效（容器内读宿主文件）==='
docker exec $NAME cat /usr/share/nginx/html/index.html 2>/dev/null || echo '(exec 失败：只读+无特权下可能不可用)'

echo '=== 5. 环境变量 ==='
docker exec $NAME printenv FOO 2>/dev/null || echo '(取不到)'

echo '=== 6. 日志（前 3 行）==='
docker logs $NAME 2>&1 | head -3

echo '=== 7. 停止并清理 ==='
docker stop $NAME >/dev/null 2>&1
echo "停止后仍在运行: $(docker ps --filter name=$NAME -q | wc -l)（应为 0）"
docker network rm $NET >/dev/null 2>&1