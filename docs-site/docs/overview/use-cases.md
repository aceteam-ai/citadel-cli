---
sidebar_position: 3
title: Use Cases
---

# Use Cases

## Agentic Execution Protocol

Some capabilities described in the Enterprise AI Factory and GPU Marketplace scenarios below are enabled by the upcoming **Agentic Execution Protocol (AEP)**, a vendor-neutral protocol for cross-organization agent invocation. AEP abstracts worker interactions into a standard protocol, allowing any compliant system to participate in the compute fabric. See the [AceTeam roadmap](https://aceteam.ai) for current status.

## Enterprise AI Factory

### Regulated Document Processing On-Premise

A mid-size financial services firm processes thousands of contracts, filings, and client communications daily. Their compliance team needs AI-powered extraction and summarization, but regulatory requirements prohibit sending client documents to third-party cloud APIs. Every document must be processed on infrastructure the firm owns and audits.

The firm installs Citadel on four GPU workstations in their existing server room. Each machine runs a large language model fine-tuned for financial document analysis. The AceTeam platform orchestrates the pipeline -- intake, extraction, summarization, and routing -- while the actual document processing happens entirely on-premise. The compliance team gets the same AI capabilities as cloud-native competitors, without any data leaving the building.

The outcome is measurable: document review time drops by 70%, the firm passes its next SOC 2 audit with no findings related to AI data handling, and the total cost of ownership is less than half of what equivalent cloud API usage would cost over 18 months.

## GPU Marketplace

### Monetizing Idle Data Center Capacity

A regional data center operator has 200 NVIDIA A100 GPUs allocated across customer workloads. On average, 40% of that capacity sits idle -- customers provision for peak load but rarely sustain it. The operator wants to monetize idle cycles without disrupting primary tenants.

By installing Citadel on their GPU servers, the operator connects idle capacity to the AceTeam compute marketplace. When a customer's primary workload is below peak, the surplus GPU time is automatically offered to the marketplace. When the primary workload scales up, Citadel gracefully drains marketplace jobs and returns full capacity to the tenant.

The operator generates new revenue from hardware that was previously earning nothing during off-peak hours. Marketplace buyers get access to enterprise-grade GPU infrastructure at competitive rates. The primary tenants experience no performance impact because the orchestration layer respects priority boundaries.

> This use case is detailed further in the [AceTeam Sovereign Compute Whitepaper](https://aceteam.ai/whitepaper).

## Consultant Multi-Tenant

### One Platform, Fifteen Clients

An AI consultancy manages custom solutions for fifteen enterprise clients. Each client has different model requirements, data sensitivity levels, and compliance obligations. The consultancy previously ran separate cloud accounts for each client, resulting in duplicated infrastructure, inconsistent tooling, and spiraling costs.

With AceTeam and Citadel, the consultancy consolidates management into a single platform. Each client's workloads run on dedicated hardware -- some on client-owned servers, some on consultancy-managed nodes -- but all are orchestrated through one control plane. Organization isolation in the AceTeam Network ensures that Client A's data and models are invisible to Client B's nodes, even though both are managed by the same team.

The consultancy reduces operational overhead by 60%. Onboarding a new client takes hours instead of weeks. Engineers work with one set of tools instead of fifteen cloud consoles. And each client retains full data sovereignty because their inference workloads run on hardware allocated exclusively to them.

## Workflow Automation

### Chaining AI Tasks Across Distributed Infrastructure

A logistics company processes shipping manifests, customs declarations, and delivery confirmations across three continents. Each document type requires a different AI model, and regulatory requirements mean European documents must be processed in the EU, Asian documents in-region, and so on.

The company deploys Citadel nodes in each region -- two servers in Frankfurt, two in Singapore, one in Chicago. Using the AceTeam platform's workflow builder, they construct an automated pipeline: incoming documents are classified, routed to the appropriate regional node for extraction, enriched with data from their ERP system, and delivered to downstream systems. The entire pipeline is defined visually in the AceTeam console and executed across the distributed Citadel nodes.

What previously required a team of three engineers maintaining custom integration code across multiple cloud providers now runs as a managed workflow. Processing time drops from hours to minutes. Regional compliance is enforced automatically by the routing rules. And when volume spikes during peak shipping season, the company adds temporary GPU nodes in each region by installing Citadel on rented bare-metal servers -- no code changes, no pipeline redesign, just more capacity joining the network.

## Edge AI and Computer Vision

### Monitoring Remote Sites with On-Device Intelligence

A cattle rancher manages herds across three remote properties, each with limited internet connectivity. Traditional cloud-based monitoring would require reliable broadband at each site -- impractical when the nearest fiber connection is 50 miles away. The rancher needs real-time herd counts, movement tracking, and alerts for animals in distress, all processed locally.

The rancher installs Citadel on a compact edge device (an NVIDIA Jetson or similar) at each property, connected to weatherproof cameras covering pastures and water points. Each node runs an object detection model (such as RF-DETR or YOLO) as a Citadel-managed Docker service. The model processes camera feeds locally -- detecting, counting, and classifying animals in real time -- without sending raw video anywhere.

Detection results (timestamped counts, movement patterns, anomaly flags) are compact structured data, typically a few kilobytes per update. Citadel relays these results through the mesh network to a central node at the ranch office, which runs a database service. Even over a low-bandwidth satellite or cellular link, the detection summaries flow reliably -- raw video stays on the edge device, only insights travel the network.

On the central node, an AceTeam workflow runs periodic analysis over the accumulated data: daily herd counts by pasture, grazing pattern trends, and alerts when a count drops unexpectedly (indicating a gate left open or an animal in trouble). The rancher checks a dashboard from anywhere, with data that is minutes old rather than days.

This same pattern applies beyond agriculture. A logistics company counts trucks at loading docks. A municipality monitors traffic flow at intersections. A warehouse tracks pallet movements. In each case, the architecture is identical: Citadel at the edge running a CV model, the mesh network carrying structured results to a central store, and workflows turning raw detections into actionable intelligence.

## Voice and Audio Generation

### Self-Hosted Creative and Production Audio

Open-source audio models have reached a tipping point. Music generation models like ACE-Step produce full songs in seconds on consumer GPUs. Text-to-speech models like Bark and XTTS clone voices with minutes of sample audio. Speech recognition models like Whisper rival commercial transcription services. All of them run locally, all of them fit in Docker containers, and all of them expose HTTP APIs.

A music production studio installs Citadel on two workstations, each with an RTX 4090. One node runs ACE-Step for music generation and Bark for vocal synthesis. The other runs Whisper for transcription and a fine-tuned XTTS model for the studio's signature voice. Through the fabric, any producer in the studio -- or remote collaborators with access to the network -- can hit these models as standard API endpoints. No cloud subscription, no per-generation fees, no audio data leaving the studio's network.

The same architecture serves production deployments. A company building a voice-enabled customer service application self-hosts its entire audio pipeline on Citadel nodes: Whisper for real-time transcription, a fine-tuned LLM for response generation, and XTTS for natural-sounding replies. The application calls these models through the fabric exactly as it would call a cloud API, but the infrastructure runs on hardware the company controls. When call volume grows, adding another GPU node to the fabric scales the pipeline without code changes.

Because Citadel treats any Docker container with an HTTP endpoint as a service, the voice and audio workflow requires no special integration. The same service management, mesh networking, and job routing that handles LLM inference handles audio generation. A `citadel.yaml` manifest that defines an ACE-Step service looks identical to one that defines a vLLM service -- different container image, same operational model.
