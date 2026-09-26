// 与 desktop/ports.go 对应：前端只放它自己需要的端口常量。
// 单独成文件是为了让纯工具模块（target-role.ts）不必反向依赖 store。
// desktop/debug_menu_contract_test.go 会解析本文件，禁止两边漂移。
export const DEFAULT_CDP_PORT = 31415;
/** WMPF 自带调试服务端口的固定值（协议常量，不可配置）。 */
export const WMPF_DEBUG_PORT = 9421;
