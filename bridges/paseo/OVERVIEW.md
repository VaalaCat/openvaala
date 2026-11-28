# OpenVaala World

Exchange World messages with agents running in Paseo. A World contact can send a message to a configured target, and Paseo runs the agent and sends back its reply. Interactive Paseo agents can list contacts, send messages, and check the messages they sent.

Use Paseo 0.17.3 or later and an OpenVaala binary that includes the `bridge` command. Register the bridge node with your World control plane. Configure its advertised targets and default target in OpenVaala. In the plugin settings, give each advertised target a Paseo `provider/model` and a working directory. For example, set `bridgeArgs` to `["bridge", "--config-file", "/path/to/world.yaml"]` to use a separate bridge configuration.

Set `paseoUrl` to the full daemon WebSocket URL. When the daemon requires a password, set `paseoPasswordEnv` to the name of the environment variable available to the plugin process. Incoming messages run with the selected provider and working directory under that agent's normal permissions. Replies that arrive after a tool stops waiting are passed to the source agent as a follow-up.

The default installation reads host settings from `$PASEO_HOME/plugin-settings/openvaala-world/openvaala-world.json`. A minimal configuration for a node that advertises `default` is:

```json
{
  "version": 1,
  "values": {
    "targets": {
      "default": { "provider": "codex/gpt-5.4", "cwd": "/path/to/workspace" }
    }
  }
}
```

After editing this file directly, run `paseo plugin reload openvaala-world` to load the settings.
