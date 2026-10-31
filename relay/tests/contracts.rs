use ormos_relay::*;
use serde::{Serialize, de::DeserializeOwned};
use serde_json::{Value, json};
use std::{
    io::{Cursor, Read},
    time::{Duration, Instant, SystemTime, UNIX_EPOCH},
};
fn roundtrip<T: Serialize + DeserializeOwned>(value: &Value) {
    let parsed: T = serde_json::from_value(value.clone()).unwrap();
    assert_eq!(serde_json::to_value(parsed).unwrap(), *value);
}
#[test]
fn control_plane_fields_match_independent_go_literals() {
    let fixtures: Vec<Value> =
        serde_json::from_str(include_str!("fixtures/go-contracts.json")).unwrap();
    assert_eq!(fixtures.len(), 11);
    for fixture in fixtures {
        let value = &fixture["value"];
        match fixture["type"].as_str().unwrap() {
            "SystemInfo" => roundtrip::<SystemInfo>(value),
            "PortInfo" => roundtrip::<PortInfo>(value),
            "ProjectInfo" => roundtrip::<ProjectInfo>(value),
            "TerminalSessionInfo" => roundtrip::<TerminalSessionInfo>(value),
            "PortEntry" => roundtrip::<PortEntry>(value),
            "DeviceStartRequest" => roundtrip::<DeviceStartRequest>(value),
            "DeviceStartResponse" => roundtrip::<DeviceStartResponse>(value),
            "DevicePollRequest" => roundtrip::<DevicePollRequest>(value),
            "DevicePollResponse" => roundtrip::<DevicePollResponse>(value),
            "ProvisionResponse" => roundtrip::<ProvisionResponse>(value),
            _ => panic!("unknown DTO fixture"),
        }
    }
    assert_eq!(DEVICE_STATUS_PENDING, "pending");
    assert_eq!(DEVICE_STATUS_EXPIRED, "expired");
    assert_eq!(DEVICE_STATUS_APPROVED, "approved");
    assert_eq!(STREAM_FENCE_VERSION, "4");
    assert_eq!(PUBLIC_KEY_HEADER, "X-Ormos-Public-Key");
    assert_eq!(STREAM_FENCE_VERSION_HEADER, "X-Ormos-Stream-Fence-Version");
}
#[test]
fn header_framing_preserves_buffered_payload_and_refuses_ambiguous_or_oversized_headers() {
    let header = StreamHeader {
        kind: "proxy".into(),
        protocol_version: "4".into(),
        system_id: "sys".into(),
        port: 8080,
        ..Default::default()
    };
    let mut wire = Vec::new();
    write_header(&mut wire, &header).unwrap();
    wire.extend_from_slice(b"next payload");
    let (got, mut reader) = read_header(Cursor::new(wire)).unwrap();
    assert_eq!(got, header);
    let mut tail = Vec::new();
    reader.read_to_end(&mut tail).unwrap();
    assert_eq!(tail, b"next payload");
    for bytes in [
        b"{\"kind\":\"event\",\"kind\":\"terminal\"}\n".as_slice(),
        b"{\"protocol_version\":\"4\",\"project_id\":\"legacy\"}\n",
        b"{\"protocol_version\":\"2\"}\n",
        b"{}",
        b"[]\n",
    ] {
        assert!(read_header(Cursor::new(bytes)).is_err());
    }
    assert!(
        read_header(Cursor::new(format!(
            "{}\n",
            " ".repeat(MAX_HEADER_SIZE + 1)
        )))
        .is_err()
    );
    let (legacy, _) =
        read_header(Cursor::new(b"{\"kind\":\"event\",\"legacy_field\":true}\n")).unwrap();
    assert_eq!(legacy.kind, "event");
    let (alias, _) = read_header(Cursor::new(
        b"{\"kind\":\"proxy\",\"Kind\":\"terminal\",\"protocol_version\":\"4\"}\n",
    ))
    .unwrap();
    assert_eq!(alias.kind, "terminal");
}
#[test]
fn version_negotiation_dimensions_and_ports_keep_wire_boundaries() {
    assert_eq!(parse_stream_fence_version_header(&[]).unwrap(), "");
    for version in ["1", "2", "3", "4"] {
        assert!(parse_stream_fence_version_header(&[version.into()]).is_ok());
    }
    for values in [
        vec!["".into()],
        vec!["0".into()],
        vec!["4,3".into()],
        vec!["4".into(), "4".into()],
    ] {
        assert!(parse_stream_fence_version_header(&values).is_err());
    }
    for (cols, rows, want) in [
        (1, 1, true),
        (1000, 1000, true),
        (0, 10, false),
        (10, -1, false),
        (1001, 10, false),
    ] {
        assert_eq!(valid_terminal_size(cols, rows), want);
    }
    assert!(valid_port(1) && valid_port(65535));
    assert!(!valid_port(0) && !valid_port(65536));
    assert_eq!(MAX_TUNNEL_WINDOW_BYTES, 512 << 20);
    assert_eq!(MAX_TUNNEL_STREAMS, 128);
}
fn terminal() -> StreamHeader {
    StreamHeader {
        kind: "terminal".into(),
        protocol_version: "4".into(),
        system_id: "sys".into(),
        terminal_record_id: "record".into(),
        terminal_generation: 3,
        cwd: "/code".into(),
        action_fence: "abcdefghijklmnopqrstuvwxyzABCDEF".into(),
        not_after_milli: 4_000_000_000_000,
        ..Default::default()
    }
}
#[test]
fn v4_identity_binding_and_shutdown_ack_cannot_cross_resources_or_fences() {
    let header = terminal();
    validate_v4_stream_header(&header, "sys").unwrap();
    assert_eq!(
        terminal_session_binding(&header).unwrap(),
        concat!(
            "v4\0sys\0record\0",
            "3\0/code\0abcdefghijklmnopqrstuvwxyzABCDEF\0",
            "4000000000000"
        )
    );
    assert_eq!(
        terminal_resource_identity(&header).unwrap(),
        concat!("v4\0sys\0record\0", "3")
    );
    assert!(validate_v4_stream_header(&header, "other").is_err());
    for field in [
        "terminal_record_id",
        "terminal_generation",
        "session_id",
        "cwd",
    ] {
        let mut bad = header.clone();
        match field {
            "terminal_record_id" => bad.terminal_record_id.clear(),
            "terminal_generation" => bad.terminal_generation = 0,
            "session_id" => bad.session_id = "legacy".into(),
            "cwd" => bad.cwd.clear(),
            _ => unreachable!(),
        }
        assert!(validate_v4_stream_header(&bad, "sys").is_err(), "{field}");
    }
    let mut shutdown = header.clone();
    shutdown.kind = "shutdown".into();
    for status in [ACTION_ACK_SUCCESS, ACTION_ACK_REFUSED, ACTION_ACK_EXPIRED] {
        let ack = new_action_ack(&shutdown, status);
        validate_action_ack(&shutdown, &ack).unwrap();
        let mut wire = Vec::new();
        write_action_ack(&mut wire, &ack).unwrap();
        assert_eq!(read_action_ack(Cursor::new(wire)).unwrap(), ack);
        let mut other = shutdown.clone();
        other.not_after_milli += 1;
        assert!(validate_action_ack(&other, &ack).is_err());
    }
    assert!(validate_action_ack(&header, &new_action_ack(&header, "success")).is_err());
    assert!(validate_action_ack(&shutdown, &new_action_ack(&shutdown, "unknown")).is_err());
}
#[test]
fn action_fence_acceptance_retains_a_monotonic_deadline_even_on_refusal() {
    let wall = UNIX_EPOCH + Duration::from_millis(1_000_000);
    let accepted = Instant::now();
    let mut header = terminal();
    header.not_after_milli = 1_001_000;
    let good = accept_stream_fence(&header, wall, accepted);
    assert_eq!(good.result, Ok(()));
    assert_eq!(good.deadline, Some(accepted + Duration::from_secs(1)));
    assert!(
        validate_stream_fence_deadline(
            good.deadline.unwrap(),
            accepted + Duration::from_millis(999)
        )
        .is_ok()
    );
    assert_eq!(
        validate_stream_fence_deadline(good.deadline.unwrap(), accepted + Duration::from_secs(1)),
        Err(FenceError::Expired)
    );
    header.not_after_milli = 999_999;
    let expired = accept_stream_fence(&header, wall, accepted);
    assert_eq!(expired.result, Err(FenceError::Expired));
    assert_eq!(expired.deadline, Some(accepted));
    header.not_after_milli = 1_061_000;
    let future = accept_stream_fence(&header, wall, accepted);
    assert!(future.result.is_err());
    assert_eq!(future.deadline, Some(accepted + Duration::from_secs(60)));
    header.action_fence = "invalid!".into();
    let invalid = accept_stream_fence(&header, wall, accepted);
    assert!(invalid.result.is_err());
    assert!(invalid.deadline.is_some());
    header.kind = "event".into();
    let event = accept_stream_fence(&header, SystemTime::now(), accepted);
    assert!(event.result.is_ok());
    assert!(event.deadline.is_none());
}
#[test]
fn terminal_frame_bytes_preserve_tags_lengths_and_bounded_decode() {
    assert_eq!(encode_data(b"abc"), [0, 0, 0, 0, 3, b'a', b'b', b'c']);
    assert_eq!(
        decode_frame(&encode_resize(132, 45)).unwrap(),
        TermFrame::Resize(Resize {
            cols: 132,
            rows: 45
        })
    );
    assert_eq!(
        decode_frame(&encode_activity(true)).unwrap(),
        TermFrame::Activity(true)
    );
    assert!(decode_frame(&[0, 0xff, 0xff, 0xff, 0xff]).is_err());
    assert!(decode_frame(&[0, 0, 0, 0, 4, 1]).is_err());
    assert!(decode_frame(&[3, 0, 0, 0, 0]).is_err());
    let mut trailing = encode_data(b"one");
    trailing.extend_from_slice(b"ignored trailing bytes");
    assert_eq!(
        decode_frame(&trailing).unwrap(),
        TermFrame::Data(b"one".to_vec())
    );
    let resize = encode_resize(-1, 1001);
    assert_eq!(
        decode_frame(&resize).unwrap(),
        TermFrame::Resize(Resize {
            cols: -1,
            rows: 1001
        })
    );
    assert_eq!(
        serde_json::to_value(ProjectInfo::default()).unwrap()["ports"],
        json!(null)
    );
}
