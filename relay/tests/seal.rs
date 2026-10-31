#![cfg(feature = "seal")]
use ormos_relay::{seal::*, *};
use serde_json::Value;
use std::io::{self, Cursor};
fn bytes(value: &Value) -> Vec<u8> {
    let value = value.as_str().unwrap();
    (0..value.len())
        .step_by(2)
        .map(|i| u8::from_str_radix(&value[i..i + 2], 16).unwrap())
        .collect()
}
#[test]
fn sealing_keys_fingerprint_and_counter_records_match_go_vectors() {
    let vector: Value = serde_json::from_str(include_str!("fixtures/go-seal-v2.json")).unwrap();
    let private = AgentKey::from_bytes(bytes(&vector["private"]).try_into().unwrap());
    assert_eq!(private.public_key().as_slice(), bytes(&vector["public"]));
    assert_eq!(fingerprint(&private.public_key()), vector["fingerprint"]);
    let keys = derive_session_keys(
        &private,
        &bytes(&vector["peer_public"]),
        &bytes(&vector["salt"]),
        vector["binding"].as_str().unwrap(),
    )
    .unwrap();
    assert_eq!(
        keys.client_to_agent.as_slice(),
        bytes(&vector["client_to_agent"])
    );
    assert_eq!(
        keys.agent_to_client.as_slice(),
        bytes(&vector["agent_to_client"])
    );
    let mut send = Sealer::new(keys.client_to_agent.as_slice()).unwrap();
    let mut recv = Sealer::new(keys.client_to_agent.as_slice()).unwrap();
    for (frame, record) in [
        encode_data("synthetic\nterminal=😀".as_bytes()),
        encode_resize(132, 45),
        encode_activity(true),
    ]
    .iter()
    .zip(vector["records"].as_array().unwrap())
    {
        let expected = bytes(record);
        assert_eq!(send.seal(frame).unwrap(), expected);
        assert_eq!(recv.open(&expected).unwrap(), *frame);
    }
    assert!(derive_session_keys(&private, &[0; 32], &[1; 32], "session").is_err());
    assert!(derive_session_keys(&private, &[1; 31], &[1; 32], "session").is_err());
    assert!(derive_session_keys(&private, &[1; 32], &[1; 31], "session").is_err());
    let other = derive_session_keys(
        &private,
        &bytes(&vector["peer_public"]),
        &bytes(&vector["salt"]),
        "different-session",
    )
    .unwrap();
    assert_ne!(
        keys.client_to_agent.as_slice(),
        other.client_to_agent.as_slice()
    );
}
#[test]
fn authenticated_records_reject_tampering_replay_and_bad_lengths() {
    let key = [9; 32];
    let mut sender = Sealer::new(&key).unwrap();
    let record = sender.seal(b"payload").unwrap();
    let mut receiver = Sealer::new(&key).unwrap();
    assert_eq!(receiver.open(&record).unwrap(), b"payload");
    assert!(receiver.open(&record).is_err());
    let mut damaged = record;
    damaged[0] ^= 1;
    assert!(Sealer::new(&key).unwrap().open(&damaged).is_err());
    assert!(read_record(&mut Cursor::new([0xff; 4])).is_err());
    assert!(read_client_hello(&mut Cursor::new([0, 0, 0, 1, 0])).is_err());
    assert!(write_client_hello(&mut Vec::new(), &[0; 31]).is_err());
    let mut hello = Vec::new();
    write_client_hello(&mut hello, &[1; 32]).unwrap();
    assert_eq!(read_client_hello(&mut Cursor::new(hello)).unwrap(), [1; 32]);
    let mut salt = Vec::new();
    write_server_hello(&mut salt, &[2; 32]).unwrap();
    assert_eq!(read_server_hello(&mut Cursor::new(salt)).unwrap(), [2; 32]);
    let mut record = Vec::new();
    write_record(&mut record, b"bytes").unwrap();
    let mut buffer = vec![0; 99];
    assert_eq!(
        read_record_into(&mut Cursor::new(record), &mut buffer).unwrap(),
        b"bytes"
    );
}
#[test]
fn decoded_frame_data_survives_subsequent_buffer_reuse_and_large_pastes() {
    let key = [7; 32];
    let mut sender = Sealer::new(&key).unwrap();
    let mut wire = Vec::new();
    for data in [b"first".to_vec(), vec![5; MAX_FRAME_SIZE], b"last".to_vec()] {
        write_record(&mut wire, &sender.seal(&encode_data(&data)).unwrap()).unwrap();
    }
    let mut stream = SealedStream::new(Cursor::new(wire), io::sink(), &key, &key).unwrap();
    let first = stream.read_frame().unwrap();
    assert_eq!(
        stream.read_frame().unwrap(),
        TermFrame::Data(vec![5; MAX_FRAME_SIZE])
    );
    assert_eq!(
        stream.read_frame().unwrap(),
        TermFrame::Data(b"last".to_vec())
    );
    assert_eq!(first, TermFrame::Data(b"first".to_vec()));
}
