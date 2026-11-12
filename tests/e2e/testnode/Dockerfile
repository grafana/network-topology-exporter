FROM alpine:3.21@sha256:bc21487101ae47172509130ee1092cc273a831968d64b617776d1f69c81e42ab

# lldpd: IEEE 802.1AB daemon with AgentX SNMP sub-agent support (-x flag).
# net-snmp: master SNMP agent (snmpd) that lldpd registers LLDP-MIB into.
RUN apk add --no-cache lldpd net-snmp net-snmp-agent-libs

COPY start.sh /start.sh
RUN chmod +x /start.sh

# SNMP on UDP 161; lldpd listens on AgentX socket /var/agentx/master (default).
EXPOSE 161/udp

CMD ["/start.sh"]
