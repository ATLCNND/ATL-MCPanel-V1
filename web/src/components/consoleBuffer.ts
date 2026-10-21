/**
 * 控制台本地缓冲的**保留口径**（纯逻辑，不依赖 React / xterm，便于单独核对）。
 *
 * 背景（用户 2026-10-17 要求）：设置里的「保留行数」应当只数**用户实际看得到的行**。
 * 原先的实现是一个总长 5000 的数组，**被屏蔽的周期性刷屏也占这个额度** ——
 * 长期运行的服务器上心跳占多数（实测 Paper 每 5 秒一条玩家数、每 15 秒一条 TPS），
 * 于是"保留 5000 行"里往往只有一小部分是有用输出，切走再切回来（会重放缓冲）
 * 看到的内容就少得莫名其妙。
 *
 * 现在改成：**按"非刷屏行"的条数裁剪**，并把被屏蔽的行一起留着 ——
 * 留着它们是必要的：用户关掉"已屏蔽刷屏"时要能立刻看到原始输出（不必重连），
 * 那正是当初把它们放进缓冲的理由（见 ConsoleTab 里 hideHeartbeat 的说明）。
 */

/** 硬上限系数：缓冲里总共最多留 `retain * 这个系数` 行。 */
export const BUFFER_HARD_FACTOR = 10

/**
 * 「保留行数」滑块的取值范围（与设置面板里的 min/max 一致）。
 * retain 会被夹到这个区间里，避免异常值把缓冲撑爆或裁到空。
 */
export const RETAIN_MIN = 1000
export const RETAIN_MAX = 50000

/**
 * 服务端**回放窗口**能接受的上限（与 internal/daemon/grpcapi/server.go 的
 * consoleHistoryMax 保持一致）。
 *
 * 为什么与 RETAIN_MAX 不是一回事：回放发生在**每一次**附加（切页签、刷新都算），
 * 一次要灌多少行受网络与首屏耗时约束，所以服务端有它自己的上限；
 * 而 RETAIN_MAX 是"本次会话里终端能滚多深"，只影响内存。
 * 界面上要按这个值如实标注，否则用户会以为"我设了 5 万却只有 5 千"。
 */
export const REPLAY_MAX = 5000

/** 把滑块的值夹成合法的保留条数。 */
export function clampRetain(scrollback: number): number {
  if (!Number.isFinite(scrollback) || scrollback <= 0) return 5000
  return Math.min(RETAIN_MAX, Math.max(RETAIN_MIN, Math.round(scrollback)))
}

/**
 * 是否需要裁剪。
 *
 * 只在"总行数超过保留条数的两倍"时才真去裁 —— 裁剪本身要扫一遍数组，
 * 每收一行就扫一次会变成 O(n²)；这样摊下来是每攒够 retain 条有用行扫一次。
 */
export function needsTrim(len: number, retain: number): boolean {
  return len > retain * 2
}

/**
 * 按"非刷屏行"的条数裁剪，返回裁剪后的数组（不修改入参）。
 *
 * 规则：
 *   ① 从**后往前**数满 retain 条 `!spam` 的行，比它更早的一律丢掉；
 *   ② 另外兜一道总行数上限（retain * BUFFER_HARD_FACTOR）：万一某段时间刷屏行
 *      是有用行的几十倍，光靠 ① 会让总行数无限涨（内存风险），这一道保证有界。
 *
 * 两条同时生效时取**更严**的那个 —— 宁可少留几行，也不让标签页把内存吃掉。
 */
export function trimByUseful<T extends { spam?: boolean }>(
  buf: T[],
  retain: number,
  hardFactor: number = BUFFER_HARD_FACTOR,
): T[] {
  if (buf.length === 0) return buf
  const keep = clampRetain(retain)
  const hardCap = keep * hardFactor

  // ① 找"第 keep 条非刷屏行"的位置（从后往前数）
  let useful = 0
  let cut = buf.length
  for (let i = buf.length - 1; i >= 0; i--) {
    if (!buf[i].spam) {
      useful++
      if (useful >= keep) {
        cut = i
        break
      }
    }
    if (buf.length - i >= hardCap) {
      // ② 总行数先到上限：以它为准
      cut = i
      break
    }
  }
  return cut === buf.length ? buf : buf.slice(cut)
}
