# 虚拟总线运行系统（vbus）

两线开漏（SCL/SDA，线与）的教学型虚拟总线。设备只能把线“拉低”或“释放”，
线电平由所有驱动意图线与得到；设备之间不直接通信，只通过电平边沿与
Bus.Emit 的协议无关事件交互。

## 时间模型

- Bus.Step() 推进一个四分之一位时钟（相位 0..3），tick 从 0 开始，
  tick%4 即相位：0=SCL 低、1=上升、2=高电平中点、3=下降。
- 一个位单元占 4 个相位；一次完整事务（START/位/ACK/STOP）都在这同一条
  tick 轴上，无“真实时间”与“仿真时间”之分。
- 无响应等待（QueueWriteWhenIdle）、重复 START（多帧之间的 Sr）、
  传输中复位（Runner.AbortReset / Runner.Recover）都只是在这条轴上
  插入不同波形与事件，不存在独立的等待线程。

## 谁在什么阶段拉哪根线

| 阶段 | SCL | SDA |
| --- | --- | --- |
| START（单元内相位 0..3） | 高，相位 3 拉低 | 高→相位 1 起拉低 |
| 地址/数据位 | 主设备按时钟拉低/释放 | 主设备驱动该位（0 拉低，1 释放） |
| ACK 位 | 主设备按时钟 | 主设备释放；被寻址从设备在相位 0..2 拉低 |
| 重复 START | 先释放 SDA、抬 SCL，SCL 高时再拉 SDA | 同 START 时序 |
| STOP | 相位 1 抬 SCL | 相位 2 释放，SCL 高时 SDA 上升 |
| 复位/恢复 | Recoverer 产生至多 9 个脉冲 | 复位后主/从设备全部释放；锁死设备由脉冲逼其释放 |

采样点：从设备在 SCL 上升沿移位移入；主设备在 ACK 位相位 2 采样
（低=ACK，高=NACK）；逐位仲裁也在每位相位 2 比较。

## 多主仲裁

- 主设备在同一位单元相位 0 同步发起时进入同一个仲裁组；总线忙时不同步
  发起者立即收到 arb-lost。
- 每位相位 2：主设备把自己的 SDA 意图与线与结果比较，意图为高、总线为
  低且低电平不是自己造成的，立即判负，调用 loseLocal 停驱动、清编排，
  Active() 立刻变 false。queued 事务保留，可用 Retry(tick) 重发。
- 仲裁组只剩一个主设备时广播 arb-grant；任意时刻至多一个主设备
  Granted()==true，不可能两个设备都认为自己独占总线。
- 地址位流完全相同的极端情形，在地址 ACK 位结束时按主设备 ID 字典序
  固定优先级破平（教学确定性规则，非真实 I2C 语义）。

## 无响应、复位与恢复

- 从设备无 ACK：主设备在 ACK 相位 2 见到高电平，置 ErrNACK，把后续
  编排裁成一个 STOP，总线回到空闲，不会永久等待。
- 发起前总线非空闲：QueueWriteWhenIdle(waitTicks, ...) 让主设备保持
  释放并逐位单元等待，超时以 ErrWaitBusy 结束。
- 传输中复位：Runner.AbortReset(who) 广播 reset，所有主设备立即中止
  （ErrReset），从设备清除寻址状态，健康设备释放两线。
- 线路恢复：Runner.Recover(who) 先发 reset 与 recovery-begin，
  Recoverer 从下一拍起在 SCL 上发至多 9 个脉冲（事件 recovery-clock），
  故障设备释放 SDA 后补一个 STOP：成功发 recovery-done 且总线空闲；
  SDA 到第 9 个脉冲后仍被锁死则发 recovery-failed，总线明确不空闲。
  异常结束后若恢复成功，线路立即可用（见测试里恢复后的第二次写）。

## 人工设备与可复现演练

ManualDevice 允许逐拍指定 Drive 并记录每个 Snapshot（含当刻
OwnersSCL/OwnersSDA），用于核对“这段电平是谁拉出来的”。

go test ./... 复现以下 9 个演练（bus/bus_test.go）：

1. TestBasicWrite 基本写 + ACK + STOP 后空闲；
2. TestArbitrationTwoMasters 两主同步发起、逐位判负、败者释放并重试；
3. TestNoResponseNACK 无设备响应时 NACK + STOP，不卡死；
4. TestWaitUntilIdle 总线忙时等待后成功，以及等待超时 ErrWaitBusy；
5. TestRepeatedStart 多帧 Sr 后从设备重新匹配地址；
6. TestResetInTransit 传输中复位，主/从释放、总线空闲；
7. TestRecoveryUnlocksSDA 故障设备锁 SDA，9 脉冲内释放、STOP 恢复并重发；
8. TestRecoveryFailsWhenLineStuck 始终锁线时 recovery-failed；
9. TestManualPhaseAudit 人工设备逐拍核对 START 归属与败者释放时刻。

## 协议支持边界

- 仅 7 位地址写事务；没有读事务、时钟拉伸（clock stretching）、通用调用
  地址（0x00）、HS/10-bit 寻址与 SMBus 超时。
- 从设备从不驱动 SCL；多主时钟同步未建模，仲裁只比较 SDA。
- 仲裁破平用固定 ID 优先级，仅为教学确定性；真实 I2C 中地址相同的两个
  主设备会在整个地址/数据阶段持续并行直到内容分叉。
- 恢复固定 9 个 SCL 脉冲 + 一次 STOP，不实现硬件复位线或带电序列。
- 设备增删只在两次 Step 之间生效（ApplyPending 在运行前已落账，
  保证首个 tick 所有设备都参与）。

