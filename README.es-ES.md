<p align="center">
  <img src="https://img.alicdn.com/imgextra/i1/O1CN01nTg6w21NqT5qFKH1u_!!6000000001621-55-tps-550-550.svg" alt="AgentScope" width="120" />
</p>

# AgentScope Go

[![CI](https://github.com/agentscope-ai/agentscope-go/actions/workflows/ci.yml/badge.svg)](https://github.com/agentscope-ai/agentscope-go/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/Go-1.25%2B-00ADD8?logo=go)](go.mod)
[![License](https://img.shields.io/badge/License-Apache--2.0-blue)](LICENSE)

[English](README.md) · [Inicio rápido](#inicio-rápido) · [Ejemplos](#ejemplos) · [Documentación](#documentación) · [Contribuir](#contribuir)

AgentScope Go es una biblioteca de Go para crear aplicaciones con modelos de
lenguaje, herramientas y flujos de trabajo con varios agentes. Implementa los
conceptos centrales de [AgentScope](https://github.com/agentscope-ai/agentscope)
mediante interfaces, contextos y canales de Go, y se puede integrar en un servicio
o en un programa de línea de comandos.

## Qué puedes crear

- **Asistentes con herramientas:** conecta llamadas al modelo con funciones de
  Go, gestiona el contexto y solicita confirmación para ejecutar herramientas.
  Usa [AskUser](docs/tools.md#askuser) para recoger respuestas estructuradas
  mediante tu propia interfaz.
- **Flujos con varios agentes:** coordina agentes mediante pipelines,
  enrutamiento de mensajes y equipos de líder y trabajadores.
- **Aplicaciones con memoria:** combina recuperación de información, estado de
  conversación y middleware de memoria con tus propias fuentes de datos.
- **Servicios y experimentos:** ofrece un servicio HTTP o una interfaz web,
  registra respuestas del modelo, evalúa ejecuciones e inspecciona trazas.

## Inicio rápido

Necesitas **Go 1.25+** y, para el programa siguiente, una clave de API de Anthropic
y un identificador de modelo disponible. Hay otros adaptadores en
[Proveedores de modelos](#proveedores-de-modelos).

### Instalar en tu aplicación

Instala la última versión etiquetada desde la ruta del módulo de la comunidad.
Las aplicaciones que usan `github.com/alanfokco/agentscope-go/v2` deben cambiar
el prefijo de sus imports; consulta las [notas de migración del módulo](CHANGELOG.md#changed--repository-and-module-path-move).

Desde un directorio nuevo:

```bash
mkdir agentscope-demo
cd agentscope-demo
go mod init example.com/agentscope-demo
go get github.com/agentscope-ai/agentscope-go/v2@latest
```

### Crear un agente

Guarda este programa como `main.go`:

```go
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

func main() {
	cm, err := model.NewAnthropicChatModel(&model.AnthropicConfig{
		SecretAPIKey:    model.NewSecretStr(os.Getenv("ANTHROPIC_API_KEY")),
		Model:           os.Getenv("ANTHROPIC_MODEL"),
		MaxOutputTokens: 1024,
	})
	if err != nil {
		log.Fatal(err)
	}

	assistant := agent.NewUnifiedAgent(
		"assistant", "You are a helpful assistant. Keep answers concise.", cm,
	)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	reply, err := assistant.Reply(ctx, "What is an AI agent? Explain in one sentence.")
	if err != nil {
		log.Fatal(err)
	}
	if text := reply.GetTextContent("\n"); text != nil {
		fmt.Println(*text)
	}
}
```

Configura tu clave y un identificador de modelo disponible para tu cuenta y
ejecuta el programa. Los comandos usan sintaxis de Bash/Zsh; en PowerShell,
configura las variables con `$env:NAME = 'value'`.

```bash
export ANTHROPIC_API_KEY='your-api-key'
export ANTHROPIC_MODEL='your-model-id'
go mod tidy
go run .
```

El programa hace una petición a la API del modelo e imprime su respuesta. Para
añadir llamadas a funciones, consulta [el ejemplo de herramientas](examples/agent_v2/);
para configuración y siguientes pasos, consulta [Primeros pasos](docs/getting-started.md).

## Proveedores de modelos

Hay adaptadores para **OpenAI** (Chat Completions y Responses), **Anthropic**,
**DashScope**, **DeepSeek**, **Gemini**, **Moonshot**, **xAI** y **Ollama**.
Consulta la [configuración de proveedores](docs/model-providers.md) y el
[código de los adaptadores](pkg/agentscope/model/) para ver sus opciones y valores
predeterminados.

Las herramientas, la entrada multimodal, el razonamiento y las estadísticas de
uso dependen del proveedor y del modelo. Los métodos `ChatStream` de los
proveedores exponen fragmentos de respuesta; `UnifiedAgent.ReplyStream` expone
eventos del ciclo de ejecución y actualmente usa llamadas al modelo sin streaming
internamente.

Para modelos locales, configura la ventana de contexto y el tiempo de espera
según el servidor y el dispositivo. Las fichas describen capacidades del modelo;
no configuran el servidor. Consulta [el despliegue edge](docs/edge-deployment.md)
y [las limitaciones de modelos locales registradas](https://github.com/agentscope-ai/agentscope-go/issues/8).

## Ejemplos

Los ejemplos se ejecutan desde una copia del repositorio, por separado de la
aplicación anterior. Esta demo del pool de trabajadores usa tareas simuladas y no
necesita una clave de API:

```bash
git clone https://github.com/agentscope-ai/agentscope-go.git
cd agentscope-go
go run ./examples/agent_pool
```

Empieza con [agent_v2](examples/agent_v2/) para herramientas de función,
[model_call](examples/model_call/) para streaming directo del modelo, o
[webui](examples/webui/) para una interfaz web. Consulta el código de cada ejemplo
para ver el modelo elegido, las variables de entorno y los servicios o runtimes
necesarios.

<details>
<summary>Ver todos los ejemplos</summary>

| Ejemplo | Contenido |
|---|---|
| [simple](examples/simple/) | Agente personalizado con una llamada al modelo |
| [agent_v2](examples/agent_v2/) | UnifiedAgent con una herramienta de función |
| [react_tool](examples/react_tool/) | Una FunctionTool personalizada |
| [react_builtin_tools](examples/react_builtin_tools/) | Herramientas integradas de programación |
| [streaming](examples/streaming/) | Eventos del ciclo de ejecución del agente |
| [ask_user](examples/ask_user/) | Preguntas estructuradas con un modelo y una respuesta del anfitrión simulados |
| [console](examples/console/) | Chat en terminal y confirmación de herramientas |
| [model_call](examples/model_call/) | Streaming del modelo, herramientas y salida estructurada |
| [structured_output](examples/structured_output/) | Salida estructurada mediante llamadas a herramientas |
| [multi_provider](examples/multi_provider/) | Configuración de proveedores y fichas de modelos |
| [multimodal](examples/multimodal/) | Imágenes mediante URL y datos base64 |
| [multiagent](examples/multiagent/) | Conversación entre agentes |
| [multiagent_multimodal](examples/multiagent_multimodal/) | Conversación entre agentes con imágenes |
| [openai_response](examples/openai_response/) | API Responses de OpenAI |
| [middleware](examples/middleware/) | Hooks de llamadas al modelo y ejecución de herramientas |
| [permission](examples/permission/) | Modos de permisos para herramientas |
| [tracing](examples/tracing/) | Trazas con LoggerTracer |
| [tracing_otlp](examples/tracing_otlp/) | Patrón de configuración para integrar OTLP |
| [agent_loop](examples/agent_loop/) | Configuración del bucle y métricas |
| [embedding](examples/embedding/) | Embeddings de texto y similitud |
| [long_term_memory](examples/long_term_memory/) | Middleware de memoria a largo plazo |
| [agentic_memory](examples/agentic_memory/) | Memoria en archivos y MEMORY.md |
| [rag_react](examples/rag_react/) | Recuperación con un índice en memoria |
| [pipeline_multi_agent](examples/pipeline_multi_agent/) | Coordinación con Pipeline y MsgHub |
| [agent_team](examples/agent_team/) | Coordinación de agentes líder y trabajadores |
| [mcp](examples/mcp/) | Descubrimiento y llamadas a herramientas MCP |
| [a2a_http](examples/a2a_http/) | Comunicación entre agentes por HTTP |
| [grpc_a2a](examples/grpc_a2a/) | Mensajería TCP con JSON delimitado por líneas; no usa gRPC |
| [replay](examples/replay/) | Registros de respuestas simuladas y persistencia en archivos |
| [replayview](examples/replayview/) | Visor de registros RunJSONL en terminal |
| [rundiff](examples/rundiff/) | Comparación de registros RunJSONL |
| [eval_harness](examples/eval_harness/) | Evaluación de respuestas de prueba registradas |
| [agent_pool](examples/agent_pool/) | Pool de trabajadores limitado con tareas simuladas |
| [hotreload](examples/hotreload/) | Recarga de configuración con tipos |
| [bench](examples/bench/) | Pruebas de carga e informes de latencia |
| [quality_load](examples/quality_load/) | Calidad de tareas con llegadas programadas y respuestas simuladas |
| [managed_inference](examples/managed_inference/) | Admisión compartida y registro de solicitudes con un servidor HTTP simulado |
| [qdrant_filter](examples/qdrant_filter/) | Filtros de metadatos con un servidor Qdrant local |
| [wasm_sandbox](examples/wasm_sandbox/) | Detección de runtimes WASM y configuración del sandbox |
| [hub_install](examples/hub_install/) | Registros de componentes y API de instalación |
| [skill_partitions](examples/skill_partitions/) | Directorios de habilidades por agente |
| [workspace_sharing](examples/workspace_sharing/) | Vínculos entre sesiones y espacios de trabajo y acceso a archivos |
| [access_control](examples/access_control/) | Concesión y comprobación de acceso a recursos |
| [document_parser](examples/document_parser/) | Análisis y división de documentos en fragmentos |
| [audit_logging](examples/audit_logging/) | Comprobaciones de políticas y registros de auditoría |
| [guardrail](examples/guardrail/) | Bloqueo, ocultación y avisos con respuestas simuladas |
| [spend_cap](examples/spend_cap/) | Presupuestos basados en el coste observado con un modelo simulado |
| [agent_service](examples/agent_service/) | Servicio HTTP y eventos SSE |
| [webui](examples/webui/) | Interfaz web integrada |
| [dingtalk_channel](examples/dingtalk_channel/) | Mensajes y confirmaciones en DingTalk |
| [scheduled_task](examples/scheduled_task/) | Tareas únicas y recurrentes |
| [realtime_echo](examples/realtime_echo/) | Cliente de eco de la interfaz de tiempo real |
| [edge_offline](examples/edge_offline/) | Enrutamiento entre nube y Ollama local |
| [edge_sensor](examples/edge_sensor/) | Middleware de sensores con un sensor simulado |
| [edge_serial_robot](examples/edge_serial_robot/) | Herramientas de dispositivos con puerto serie simulado |
| [edge_fleet](examples/edge_fleet/) | Simulación de flota con pub/sub en memoria |
| [werewolves](examples/werewolves/) | Juego de Hombres Lobo con varios agentes |
| [k8s_workspace](examples/k8s_workspace/) | Configuración de espacios de trabajo y herramientas de Kubernetes |

</details>

La [guía de ejemplos](docs/examples.md) contiene instrucciones de ejecución y
enlaces al mismo catálogo.

## Documentación

| Tema | Guía |
|---|---|
| Configuración y primer agente | [Primeros pasos](docs/getting-started.md) |
| Modelos y herramientas | [Proveedores](docs/model-providers.md) · [Herramientas](docs/tools.md) |
| Middleware y memoria | [Middleware](docs/middleware.md) |
| Despliegue de aplicaciones | [Despliegue](docs/deployment.md) · [Límites de ejecución y sesiones](docs/adversarial-hardening.md) |
| Runtime y evaluación | [Funciones del runtime](docs/go-exclusive.md) · [Pruebas de carga](docs/benchmarks.md) · [Código de replay y evaluación](pkg/agentscope/replay/) |
| Modelos locales y dispositivos | [Despliegue edge](docs/edge-deployment.md) · [Herramientas de dispositivos](docs/device-tools.md) · [Coordinación entre dispositivos](docs/multi-device.md) · [Funcionamiento sin conexión](docs/offline-operation.md) |
| Implementación y compatibilidad | [Mapa del código](CLAUDE.md) · [Estabilidad de API](STABILITY.md) · [Historial de cambios](CHANGELOG.md) |

## Estado del proyecto

La estabilidad varía según el paquete. Las API centrales de modelos, mensajes,
agentes, herramientas, permisos, formateadores y errores tienen compromisos de
estabilidad documentados; otros paquetes incluyen interfaces experimentales.
Consulta [STABILITY.md](STABILITY.md) para conocer su alcance exacto y el trabajo
pendiente. Las funciones de `main` pueden no estar en una versión publicada.

Si tu despliegue ejecuta comandos o accede a archivos, configura y comprueba el
backend del espacio de trabajo y sus permisos. Las comprobaciones de permisos por
sí solas no ofrecen aislamiento completo de procesos, red o recursos. Los
requisitos del despliegue dependen de los proveedores y backends elegidos.

## Contribuir

Son bienvenidos los informes de errores, pruebas de regresión, mejoras de
documentación y funciones con un alcance concreto. Para un error, incluye la
versión o el commit, la configuración y una reproducción mínima sin secretos.
Para una función más amplia, abre una issue para discutir el caso de uso y la API
antes de emprender una implementación extensa.

Lee [CONTRIBUTING.md](CONTRIBUTING.md) para la configuración y el proceso de PR,
y [AGENTS.md](AGENTS.md) para los requisitos de validación y revisión. Los PR
pequeños con un propósito claro son más fáciles de revisar; no es necesario
abordar un subsistema completo. Sigue nuestro [Código de conducta](CODE_OF_CONDUCT.md).

Comunica posibles vulnerabilidades de forma privada mediante [SECURITY.md](SECURITY.md),
no en una issue pública.

## Licencia y cita

Licencia [Apache-2.0](LICENSE). Si utilizas AgentScope en investigación, consulta
[AgentScope: A Flexible yet Robust Multi-Agent Platform](https://arxiv.org/abs/2402.14034).
