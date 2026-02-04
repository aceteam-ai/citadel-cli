---
sidebar_position: 3
title: Use Cases
---

# Use Cases

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
