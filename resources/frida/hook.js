const VERBOSE = false;
const PLATFORM = Process.platform;  // 'darwin' | 'windows'
const ARCH = Process.arch;          // 'arm64' | 'x64' | 'x86_64' | ...

// The miniapp dials this port by convention and it cannot be changed. Newer
// flue builds read the address out of the launch config instead of hardcoding
// it, so the hook has to write it there — must match DEBUG_PORT in
// core/src/engine/wmpf-frida-runtime.ts.
const WEBSOCKET_URL = "ws://localhost:9421";

// ── Module Resolution ──

const getMainModule = (version) => {
    if (PLATFORM === "darwin") {
        return Process.findModuleByName("WeChatAppEx Framework");
    }
    // Windows: flue.dll for newer versions
    if (version >= 13331) {
        return Process.findModuleByName("flue.dll");
    }
    return Process.findModuleByName("WeChatAppEx.exe");
};

// ── Scene Values ──

const SCENE_WHITELIST = [
    1005, 1007, 1008, 1012, 1027, 1035, 1053, 1074,
    1145, 1168, 1178, 1256, 1260, 1302, 1308,
];

// ── CDP Filter Patch (two layouts) ──

// Newer flue builds hand the devtools payload to CastToJson, so the filter
// function has to be replaced by a shim that still calls through to it.
const replaceCDPFilter = (base, config) => {
    // xref: CastToJson (see upstream PR #262)
    const castToJson = new NativeFunction(
        base.add(config.CastToJsonHookOffset),
        "pointer",
        ["pointer", "pointer"],
    );
    const filter = new NativeCallback(function (thiz, jsonOut, cborInput) {
        castToJson(jsonOut, cborInput);
        return jsonOut;
    }, "pointer", ["pointer", "pointer", "pointer"]);
    Interceptor.replace(base.add(config.CDPFilterHookOffset), filter);
};

// Older builds: watch the filter and clear the marker that blocks devtools.
const watchCDPFilter = (base, offset) => {
    // xref: SendToClientFilter / devtools_message_filter_applet_webview.cc
    Interceptor.attach(base.add(offset), {
        onEnter(args) {
            if (PLATFORM === "windows") {
                this.inputValue = args[0];
            }
        },
        onLeave(retval) {
            let target;
            // The whole read is guarded: a wrong detector candidate (Windows)
            // or a wrong table offset (macOS) must stay inert instead of
            // throwing out of the hook.
            try {
                if (PLATFORM === "darwin") {
                    // macOS: retval points directly to the struct
                    if (retval.isNull()) return;
                    target = retval.add(8);
                } else {
                    // Windows: dereference args[0] to get the struct pointer
                    const ptr = this.inputValue.readPointer();
                    if (ptr.isNull()) return;
                    target = ptr.add(8);
                }
                VERBOSE && console.log(`[patch] CDP filter v8[2]: ${target.readU32()}`);
                if (target.readU32() === 6) {
                    target.writeU32(0x0);
                }
            } catch (e) {
                return;
            }
        },
    });
};

const patchCDPFilter = (base, config) => {
    if (config.CastToJsonHookOffset) {
        replaceCDPFilter(base, config);
        return;
    }
    // Detected Windows builds narrow the filter to several candidates
    // (CDPFilterHookOffsets); every one is watched and the === 6 runtime guard
    // makes wrong candidates inert.
    const offsets = config.CDPFilterHookOffsets ?? [config.CDPFilterHookOffset];
    offsets.forEach((offset) => watchCDPFilter(base, offset));
};

// ── OnLoadStart ──

// Newer flue builds moved the debug socket address out of the binary and into
// the launch config. Without writing it there the miniapp never dials our
// listener, so this is what makes 9421 work on those builds.
const enableRemoteDebug = (launchConfigPtr, remoteDebugConfigPtr, struct) => {
    const stringPtr = launchConfigPtr.add(struct.WebSocketURLStringOffset);
    // C++ small-string optimisation: byte 23 holds the length, and a negative
    // value there means the 23 inline bytes are a {pointer, length, capacity}
    // triple instead.
    if (stringPtr.add(23).readS8() < 0) {
        stringPtr.readPointer().writeUtf8String(WEBSOCKET_URL);
        stringPtr.add(8).writeU64(WEBSOCKET_URL.length);
    } else {
        stringPtr.writeUtf8String(WEBSOCKET_URL);
        stringPtr.add(23).writeU8(WEBSOCKET_URL.length);
    }
    send(`[hook] websocket url -> ${WEBSOCKET_URL}`);

    const modePtr = remoteDebugConfigPtr.add(struct.RemoteDebugModeOffset);
    send(`[hook] remote debug mode: ${modePtr.readInt()} -> 1`);
    modePtr.writeInt(1);
};

// One hook for both platforms: Frida resolves args[] against the running ABI
// (Microsoft RDX / darwin arm64 x1 / darwin x64 RSI), so naming a context
// register here would silently target the wrong argument on the other one.
// The debug flag is the second argument, the launch struct the first.
//
// Two struct layouts: a table carries either six SceneOffsets (older builds) or
// a MiniAppConfigStructOffsets object (newer ones, which also need the remote
// debug setup above).
const patchLoadStart = (base, config) => {
    // xref: AppletIndexContainer::OnLoadStart
    const struct = config.MiniAppConfigStructOffsets;
    Interceptor.attach(base.add(config.LoadStartHookOffset), {
        onEnter(args) {
            if (args[1].and(0xff).toInt32() !== 1) {
                // Clear the low byte by subtracting it rather than masking with
                // 0xffffffffffffff00: that literal is not exactly representable
                // as a JS number, and Frida takes masks as numbers.
                args[1] = args[1].sub(args[1].and(0xff)).or(1);
                send(`[hook] debug flag -> ${args[1]}`);
            }
            // Scene hijack via the launch-config pointer chain
            try {
                let launchConfigPtr;
                let remoteDebugConfigPtr;
                let scenePtr;
                if (struct) {
                    launchConfigPtr = args[0]
                        .add(struct.LaunchConfigOffsets[0])
                        .readPointer()
                        .add(struct.LaunchConfigOffsets[1])
                        .readPointer()
                        .add(struct.LaunchConfigOffsets[2])
                        .readPointer();
                    remoteDebugConfigPtr = launchConfigPtr
                        .add(struct.RemoteDebugConfigOffsets[0])
                        .readPointer()
                        .add(struct.RemoteDebugConfigOffsets[1])
                        .readPointer();
                    scenePtr = remoteDebugConfigPtr.add(struct.SceneOffset);
                } else {
                    const offsets = config.SceneOffsets;
                    launchConfigPtr = args[0]
                        .add(offsets[0])
                        .readPointer()
                        .add(offsets[1])
                        .readPointer();
                    remoteDebugConfigPtr = launchConfigPtr
                        .add(offsets[2])
                        .readPointer()
                        .add(offsets[3])
                        .readPointer()
                        .add(offsets[4])
                        .readPointer();
                    scenePtr = remoteDebugConfigPtr.add(offsets[5]);
                }
                const scene = scenePtr.readInt();
                send(`[hook] scene: ${scene}`);
                // Only entry points known to survive the debug flag get hijacked;
                // the remote-debug setup below inherits that same decision.
                if (!SCENE_WHITELIST.includes(scene)) return;
                send("[hook] hook scene -> 1101");
                scenePtr.writeInt(1101);
                if (struct) {
                    enableRemoteDebug(launchConfigPtr, remoteDebugConfigPtr, struct);
                }
            } catch (e) {
                send(`[hook] scene hook error: ${e}`);
            }
        },
        onLeave() {},
    });
};

// ── Config Parsing ──

const parseConfig = () => {
    const rawConfig = `@@CONFIG@@`;
    if (rawConfig.includes("@@")) {
        // Fallback test config: mirrors whatever the shipped tables carry, so a
        // script run by hand against a real client still hooks the right build.
        if (PLATFORM === "darwin") {
            return {
                Version: 269136,
                Arch: {
                    arm64: {
                        LoadStartHookOffset: "0x4F744C4",
                        CDPFilterHookOffset: "0x8436B98",
                        SceneOffsets: [56, 1504, 8, 1440, 16, 456],
                    },
                },
            };
        }
        return {
            Version: 18955,
            LoadStartHookOffset: "0x25B52C0",
            CDPFilterHookOffset: "0x30248B0",
            SceneOffsets: [56, 1408, 8, 1344, 16, 488],
        };
    }
    return JSON.parse(rawConfig);
};

const resolveArchConfig = (config) => {
    // Windows configs have offsets at top level — return directly
    if (config && config.LoadStartHookOffset) {
        return config;
    }
    // Mac configs nest under Arch.{arm64|x64}: a build is only hooked on an
    // arch whose entry exists, so an unlisted arch stays dormant instead of
    // patching garbage with the other arch's offsets.
    const table = config?.Arch || config?.arch;
    if (!table) return null;

    const candidates = [ARCH];
    if (ARCH === "x86_64") candidates.push("x64");
    if (ARCH === "amd64") candidates.push("x64");

    for (const key of candidates) {
        const picked = table[key];
        if (picked && picked.LoadStartHookOffset) {
            return { ...picked, Version: config.Version, __arch: key };
        }
    }
    return null;
};

// ── Main ──

const main = () => {
    const rawConfig = parseConfig();
    const config = resolveArchConfig(rawConfig);

    if (!config) {
        console.error(`[frida] no config for platform=${PLATFORM} arch=${ARCH}`);
        return;
    }

    const mainModule = getMainModule(config.Version);
    if (!mainModule) {
        const expected = PLATFORM === "darwin" ? "WeChatAppEx Framework" : "flue.dll / WeChatAppEx.exe";
        console.error(`[frida] module not found: ${expected}`);
        return;
    }

    console.error(`[frida] version=${config.Version} platform=${PLATFORM} arch=${config.__arch || ARCH}`);
    console.error(`[frida] module base: ${mainModule.base}`);

    const base = mainModule.base;

    // CDP filter first — main()'s callers rely on OnLoadStart being the last
    // thing attached.
    patchCDPFilter(base, config);
    patchLoadStart(base, config);
};

main();
