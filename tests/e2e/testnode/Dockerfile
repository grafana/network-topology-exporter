FROM alpine:3.24@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf

# lldpd: IEEE 802.1AB daemon with AgentX SNMP sub-agent support (-x flag).
# net-snmp: master SNMP agent (snmpd) that lldpd registers LLDP-MIB into.
RUN apk add --no-cache lldpd net-snmp net-snmp-agent-libs

COPY start.sh /start.sh
RUN chmod +x /start.sh

# SNMP on UDP 161; lldpd listens on AgentX socket /var/agentx/master (default).
EXPOSE 161/udp

CMD ["/start.sh"]
