# TC-Connect 开发任务说明

## 全局信息
项目根目录: /media/ubuntu/data/gitSourceCode/AIWorkflow/tc-connect

## 项目目标
开发一个连接飞书和多个子Agent的中间控制层的功能，实现AI工作流自动化编排。

## 工作流程
1. 用户在飞书输入：`/harness <具体功能>`
2. 主Agent接收请求并按协议格式输出给tc-connect
3. tc-connect解析主Agent输出，提取agent_name和参数
4. tc-connect根据agent_name找到对应提示词路径，启动对应子Agent并传递参数
5. 子Agent执行完毕后返回结果给tc-connect
6. tc-connect将agent_name和结果/错误返回给主Agent继续后续步骤
7. 重复步骤3-6，直到主Agent输出结束标志

## 协议格式
主Agent输出格式规范：
- 格式：[AGENT_NAME]||[PARAMS]||[RESUME]
- 说明：
  - AGENT_NAME: 子Agent唯一标识，用于查找提示词路径，並啓動對應子服務
  - PARAMS: 指导子Agent执行的简单语句和文件路径信息
  - RESUME: true或者false，true表示使用上一次的session，用于问题溯源
- 示例：
  - planner||生成项目规划文档||false
  - coder||实现用户登录模块，參考規劃文件:/docs/plan.md||false
  - tester||測試用户登录模块||false
  - coder||登錄模塊出現異常，查看/test/2026_0602_1750.log中的核心報錯解決||true
  - END 任务完成

## 系统要求
1. tc-connect只需解析主Agent输出内容
2. 不需要给主Agent提供agent字段，避免过大压力
3. 子提示词不要输出大段内容
4. session维护由tc-connect负责
5. 避免文件读写功能消耗主Agent上下文
6. tc-connect根据agent_name从本地目录/subagent查找提示词路径

## 风险控制
1. 子Agent卡死处理：
   - 启动超时检测
   - 超时则杀死进程
   - 重试3次
   - 返回错误给飞书用户层
2. Agent产出内容超限告警：
   - 说明提示词设计不合理
   - 或大模型异常

## 实现要求
1. 模块化设计，便于后续扩展
2. 完善的错误处理机制
3. 清晰的日志记录
4. 可配置的超时和重试策略
5. 支持并发处理多个任务

## 代码结构建议
- 主入口模块
- 飞书适配器模块
- 协议解析器模块
- 子Agent管理器模块
- 会话管理器模块
- 配置管理模块
- 日志和监控模块

## 注意事项
1. 保持代码简洁，避免过度设计
2. 提供详细的注释和文档
3. 考虑性能和可扩展性
4. 确保代码可测试性
5. 遵循Python最佳实践

请根据以上要求生成或更新tc-connect代码，实现完整的功能流程。