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

`bus/bus_test.go` 与 `bus/repeated_start_test.go` 覆盖五个场景：

- `TestWriteAckAndManualPhaseOwnership`：人工观察 START、地址 ACK、数据 ACK 和 STOP 的线路归属。
- `TestNACKFinishesWithStop`：无地址响应时在 ACK 相位 1 判定 NACK，随后发 STOP，不永久等待。
- `TestArbitrationLoserWaitsRetriesAndOnlyWinnerOwnsBus`：双主同时发起，输方在分岔拍释放并保留原帧，观察胜方完成、空闲位周期后再重试。
- `TestResetAndNineClockRecoveryReturnsBusToIdle`：传输中复位中止主设备，恢复源输出 9 个 SCL 脉冲并以 STOP 释放总线；人工设备在第 9 脉冲后释放 SDA。
- `TestRepeatedStartBetweenFrames`：两帧之间只发重复 START，不产生中途 STOP，从设备重新按地址匹配。

## 协议支持边界

- 支持开漏线与、START、重复 START、STOP、7 位地址写、逐字节 ACK/NACK 和 SDA 仲裁。
- 仲裁只比较主设备发送的数据位；地址 ACK、数据 ACK、START 和 STOP 不参与仲裁。
- 输方在当前快照观察结束后收到 `arb-lost`，从下一相位开始完全释放线路，并保留原 `Frame` 与失败时间作为重试依据。
- 无响应不是无限等待：主设备在该 ACK 的采样相位得到 NACK，截断后续编排并发出 STOP。
- 传输中复位、9 脉冲恢复和 STOP 都由同一个离散 `Runner.Step`/`Runner.Run` 时间过程推进。
- 恢复只验证协作式或人工设备最终释放 SDA；若设备永久强拉 SDA，模型会如实保持非空闲，不能虚构恢复成功。

当前不包含读传输、10 位地址、时钟拉伸、多主控动态热插拔、毛刺过滤、真实墙钟超时、SMBus/PMBus 扩展和并发 goroutine 调度。
