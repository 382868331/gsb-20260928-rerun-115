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

`bus/bus_test.go`、`bus/repeated_start_test.go` 与 `bus/group_test.go` 覆盖以下场景：

- `TestWriteAckAndManualPhaseOwnership`：人工观察 START、地址 ACK、数据 ACK 和 STOP 的线路归属。
- `TestNACKFinishesWithStop`：无地址响应时在 ACK 相位 1 判定 NACK，随后发 STOP，不永久等待。
- `TestArbitrationLoserWaitsRetriesAndOnlyWinnerOwnsBus`：双主同时发起，输方在分岔拍释放并保留原帧，观察胜方完成、空闲位周期后再重试。
- `TestResetAndNineClockRecoveryReturnsBusToIdle`：传输中复位中止主设备，恢复源输出 9 个 SCL 脉冲并以 STOP 释放总线；人工设备在第 9 脉冲后释放 SDA。
- `TestRepeatedStartBetweenFrames`：两帧之间只发重复 START，不产生中途 STOP，从设备重新按地址匹配。
- `TestGroupHoldsBusWithSlavePauseResumeAndQuit`：主机用 `QueueGroup` 连续完成写-读-写三个操作，中间只发重复 START、保持占用；从设备按 `SlaveScript` 在读位上拉低 SCL 暂停两个位周期后接续，并在第三次被寻址时不应答、从总线摘除自己。组内每个操作保留独立 `OpResult`，第三操作失败不抹掉前两个已确认结果。
- `TestGroupContentionLoserResumesWithoutResending`：两组操作争用，第一操作双方完全一致（均被确认），第二操作分岔出胜负；失败方重试时只从未确认的操作继续，已确认的写入不会重发。
- `TestExceptionAbortAndWait`：更高优先级异常请求以 `EvException` 广播：`abort` 在当前操作边界截断本组并发 STOP，`wait` 仅登记等待、本组照常完成。
- `TestCheckpointContinueAndManualAbortHandoff`：在组内任意一位用 `Runner.Checkpoint` 保存后继续，波形与无保存运行逐条一致；人工 `AbortGroup` 放弃剩余操作后，一直报告 `waiting-bus` 的另一主机在 STOP 之后取得线路并完成自己的操作。

## 组传输与状态

- `Op` 描述组内一次读或写；`Master.QueueGroup` 把一组操作编排为 START + 若干重复 START + 一次 STOP，全程占用线路。
- `TxResult.Ops` 给出每个操作独立的确认标志、读回数据和错误；`Master.ConfirmedOps` 是已确认的前缀操作数。
- `Master.Status` 报告 `waiting-bus`（等待线路）、`driving`、`waiting-device`（从设备拉伸时钟）、`lost-occupancy`（已失去占用）等状态。
- 从设备时钟拉伸以整位周期插入，主机把延迟对齐到整位边界后重放本位；`SlaveScript.QuitAtAddr` 让从设备在第 N 次被寻址时不应答并退出总线。
- 主机只在总线未被占用（START 与 STOP 之间为占用）时发起 START；瞬时两线皆高不等于空闲。
- `Runner.Checkpoint` 是只读保存点：记录拍号、波形长度、各主机已确认操作数与各从设备已收字节数，不影响后续推进。

## 协议支持边界

- 支持开漏线与、START、重复 START、STOP、7 位地址读写、逐字节 ACK/NACK、SDA 仲裁、整位周期的从设备时钟拉伸和组内多操作占用。
- 仲裁只比较主设备发送的数据位；地址 ACK、数据 ACK、START 和 STOP 不参与仲裁。
- 输方在当前快照观察结束后收到 `arb-lost`，从下一相位开始完全释放线路，并保留原 `Frame` 与失败时间作为重试依据。
- 无响应不是无限等待：主设备在该 ACK 的采样相位得到 NACK，截断后续编排并发出 STOP。
- 传输中复位、9 脉冲恢复和 STOP 都由同一个离散 `Runner.Step`/`Runner.Run` 时间过程推进。
- 恢复只验证协作式或人工设备最终释放 SDA；若设备永久强拉 SDA，模型会如实保持非空闲，不能虚构恢复成功。

当前不包含 10 位地址、多主控动态热插拔、毛刺过滤、真实墙钟超时、SMBus/PMBus 扩展和并发 goroutine 调度。
