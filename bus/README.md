# 虚拟总线演练说明

## 时间模型

每个位单元固定为 4 个离散相位：

| 相位 | 线路动作 |
| --- | --- |
| 0 | 本位准备，SCL 低 |
| 1 | SCL 释放为高，主从设备在此采样数据或应答 |
| 2 | SCL 保持高；START/STOP 的 SDA 跳变在高电平期间形成 |
| 3 | SCL 拉低，进入下一位单元 |

设备只能拉低或释放 SCL/SDA，总线电平由所有驱动意图线与得到。快照中的 `OwnersSCL`、`OwnersSDA` 给出当前实际拉线设备。

## 可复现演练

运行 `go test ./...`。

测试覆盖正常传输、仲裁、从设备故障、恢复争用和中途检查点：

- `TestWriteAckAndManualPhaseOwnership`：人工观察 START、地址 ACK、数据 ACK 和 STOP 的线路归属。
- `TestNACKFinishesWithStop`：无地址响应时在 ACK 相位 1 判定 NACK，随后发 STOP，不永久等待。
- `TestArbitrationLoserWaitsRetriesAndOnlyWinnerOwnsBus`：双主同时发起，输方在分岔拍释放并保留原帧，观察胜方完成、空闲位周期后再重试。
- `TestResetAndNineClockRecoveryReturnsBusToIdle`：传输中复位中止主设备，恢复源输出 9 个 SCL 脉冲并以真实 STOP 释放总线；人工设备在第 9 脉冲后释放 SDA。
- `TestRepeatedStartBetweenFrames`：两帧之间只发重复 START，不产生中途 STOP，从设备重新按地址匹配。
- `TestSlaveClockStretchDelaysButPreservesTransfer`：从设备在 SCL 相位 1 保持 SCL 低，主设备记录 `clock-wait` 并等待真实上升沿；位序和接收字节与正常传输一致。
- `TestSlaveFaultClearsAndTransferResumesWithoutBitLoss`：从设备同时拉长 SCL 和保持 SDA，故障到点解除后传输继续完成，总线最终空闲。
- `TestSlaveDataHoldRecoveryAndTimeoutDoNotStartNewTransfer`：超过主设备等待上限时记录 `clock-timeout` 并中止事务，解除故障不会偷偷重启已失败事务。
- `TestMultipleRecoveriesOnlyOneCanOwnBus`：多个恢复源同 tick 恢复时，只有一个获得 `recovery-grant`，其它恢复源记录 `recovery-lost`，不会各自宣布成功。
- `TestFailedRecoveryBlocksNewTransfer`：9 个时钟后 SDA 仍被强拉时记录 `recovery-failed`，没有 STOP，主设备保持阻塞，不能开始新传输。
- `TestCheckpointMidBitMatchesContinuousWaveAndArbitration`：在一个位单元中间保存，多推进一拍后恢复；最终电平轨迹、仲裁失败拍和接收结果与连续模拟一致。

## 协议支持边界

- 支持开漏线与、START、重复 START、STOP、7 位地址写、逐字节 ACK/NACK 和 SDA 仲裁。
- 支持从设备确定性故障脚本 `SlaveFault{StretchAt, StretchTicks, HoldAt, HoldTicks, ClearAt}`，可分别制造 SCL 时钟拉伸、传输中 SDA 保持和到点解除。
- 主设备只根据线与后的 SCL/SDA 推进；自己在相位 1 释放 SCL 不代表线路已经变高。等待开始记录 `clock-wait`，等待超限记录 `clock-timeout`。
- `Bus.Checkpoint()`/`Bus.Restore()` 可在两次 Tick 之间保存和恢复电气电平、事件轨迹、待派发事件以及设备内部状态；恢复后按相同驱动过程继续。
- 仲裁只比较主设备发送的数据位；地址 ACK、数据 ACK、START 和 STOP 不参与仲裁。
- 从设备故障保持 SDA 不会被误认为另一台主设备赢得仲裁；仲裁器只检查 SDA 的实际主控占有者。
- 输方在当前快照观察结束后收到 `arb-lost`，从下一相位开始完全释放线路，并保留原 `Frame` 与失败时间作为重试依据。
- 无响应不是无限等待：主设备在该 ACK 的采样相位得到 NACK，截断后续编排并发出 STOP。
- 传输中复位、9 脉冲恢复和 STOP 都由同一个离散 `Runner.Step`/`Runner.Run` 时间过程推进。
- 多个恢复源可用 `NewRecoveryGroup` 互斥；恢复只在最终真实观察到 STOP 时记录 `recovery-done`。若设备永久强拉 SDA，模型记录 `recovery-failed` 并保持非空闲，不能虚构恢复成功。

当前不包含读传输、10 位地址、时钟拉伸、多主控动态热插拔、毛刺过滤、真实墙钟超时、SMBus/PMBus 扩展和并发 goroutine 调度。
