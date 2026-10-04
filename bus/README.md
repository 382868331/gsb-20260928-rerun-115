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
- `TestGroupWriteReadWriteHoldsBusAndKeepsPerOpResults`：组内写-读-写连续完成，中间只有重复 START（共 3 个 START、1 个 STOP），每个操作保留独立结果。
- `TestGroupSlavePauseResumeReportsWaitingDevice`：从设备在组中途拉低 SCL 暂停若干拍再释放接续；主设备在此期间报告“等待设备”。
- `TestGroupSlaveQuitKeepsPartialResults`：从设备最终退出（停止应答），整组以 STOP 结束，已完成操作的结果不被组失败抹掉。
- `TestGroupContentionLoserDoesNotResendAckedBytes`：两组操作争用，失败方保留已确认进度，续传时跳过已应答字节，不向从设备重发。
- `TestGroupInterruptReleasesBusForWaitingMaster`：人工放弃剩余操作后组以 STOP 提前释放，等待线路的高优先级主设备随后取得线路。
- `TestGroupSaveResumeAtArbitraryBit`：在组内任意一位保存，复位中止后继续；已完成字节不重复，输方自 arb-lost 拍起完全释放，后续仲裁一致。

## 组传输模型

- `GroupMaster` 把一组 `Op`（读或写）编排成连续位段：获得线路后中间只发重复 START，保持占用直到组末 STOP，其他主设备无法插入。
- 每个操作的结果独立保存在 `GroupResult.Ops`：已应答写字节数、已读回字节、完成标记和单操作错误互不影响。
- `GroupMaster.Status` 报告 `waiting-bus`（等待线路）、`occupied`（占用中）、`waiting-device`（等待设备，即从设备拉伸时钟）、`lost`（已失去占用）、`done`、`aborted`。
- 从设备拉伸 SCL 时，主设备整组时序逐拍顺延；`StretchSlave` 以 SCL 上升沿采样、下降沿推进读位单元，因此任意拍数的拉伸后双方仍对齐。
- 仲裁失败或保存点继续都按“已确认断点”续传：跳过已完成操作与已应答字节，已确认写入绝不重发；未确认的字节（含保存点所在的半字节）按字节粒度重发。
- `Interrupt` 在当前位段边界截断整组并补 STOP；`Save`/`ResumeAt` 支持在组内任意一位保存后继续；`start<0` 的组主设备等到总线连续空闲一个位周期才发起 START。

## 协议支持边界

- 支持开漏线与、START、重复 START、STOP、7 位地址读写、逐字节 ACK/NACK、SDA 仲裁、组内保持占用、从设备时钟拉伸与中途退出。
- 仲裁只比较主设备发送的数据位；地址 ACK、数据 ACK、START 和 STOP 不参与仲裁。
- 输方在当前快照观察结束后收到 `arb-lost`，从下一相位开始完全释放线路，并保留原 `Frame` 与失败时间作为重试依据。
- 无响应不是无限等待：主设备在该 ACK 的采样相位得到 NACK，截断后续编排并发出 STOP。
- 传输中复位、9 脉冲恢复和 STOP 都由同一个离散 `Runner.Step`/`Runner.Run` 时间过程推进。
- 恢复只验证协作式或人工设备最终释放 SDA；若设备永久强拉 SDA，模型会如实保持非空闲，不能虚构恢复成功。
- 读操作的重试从已读回字节之后继续，但从设备的读数据指针不随主设备断点联动（教学模型边界）。

当前不包含 10 位地址、多主控动态热插拔、毛刺过滤、真实墙钟超时、SMBus/PMBus 扩展和并发 goroutine 调度。
